// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ciliumidentity

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/job"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"

	cilium_api_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/k8s/resource"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
)

const (
	// defaultSyncBackOff is the default backoff period for resource workqueue errors.
	defaultSyncBackOff = 1 * time.Second
	// maxSyncBackOff is the max backoff period for resource workqueue errors.
	maxSyncBackOff = 100 * time.Second
	// maxRetries is the number of times a work queue will be retried before
	// it is dropped out of the queue.
	maxProcessRetries = 15
)

type QueuedItem interface {
	Key() resource.Key
	Reconcile(reconciler *reconciler) error
	Meter(enqueuedLatency float64, processingLatency float64, isErr bool, metrics *Metrics)
}
type queueOperation interface {
	enqueueReconciliation(item QueuedItem, delay time.Duration)
}

type params struct {
	cell.In

	Logger              *slog.Logger
	Config              config
	Lifecycle           cell.Lifecycle
	Clientset           k8sClient.Clientset
	SharedCfg           SharedConfig
	JobGroup            job.Group
	Metrics             *Metrics
	Namespace           resource.Resource[*slim_corev1.Namespace]
	Pod                 resource.Resource[*slim_corev1.Pod]
	CiliumIdentity      resource.Resource[*cilium_api_v2.CiliumIdentity]
	CiliumEndpoint      resource.Resource[*cilium_api_v2.CiliumEndpoint]
	CiliumEndpointSlice resource.Resource[*v2alpha1.CiliumEndpointSlice]
}

type Controller struct {
	logger              *slog.Logger
	clientset           k8sClient.Clientset
	reconciler          *reconciler
	jobGroup            job.Group
	metrics             *Metrics
	namespace           resource.Resource[*slim_corev1.Namespace]
	pod                 resource.Resource[*slim_corev1.Pod]
	ciliumIdentity      resource.Resource[*cilium_api_v2.CiliumIdentity]
	ciliumEndpoint      resource.Resource[*cilium_api_v2.CiliumEndpoint]
	ciliumEndpointSlice resource.Resource[*v2alpha1.CiliumEndpointSlice]

	// Work queue is used to sync resources with the api-server. It will rate-limit
	// requests going to api-server. Ensures a single resource key will not be
	// processed multiple times concurrently, and if a resource key is added
	// multiple times before it can be processed, this will only be processed once.
	resourceQueue workqueue.TypedRateLimitingInterface[QueuedItem]

	cesEnabled bool

	// oldNSSecurityLabels is a map between namespace, and it's security labels.
	// It's used to track previous state of labels, to detect when labels changed.
	oldNSSecurityLabels map[string]labels.Labels

	enqueueTimeTracker *EnqueueTimeTracker
}

func registerController(p params) {
	isOperatorManageCIDsEnabled := cmp.Or(
		p.Config.IdentityManagementMode == option.IdentityManagementModeOperator,
		p.Config.IdentityManagementMode == option.IdentityManagementModeBoth,
	)

	if cmp.Or(
		!p.Clientset.IsEnabled(),
		!isOperatorManageCIDsEnabled,
		p.SharedCfg.DisableNetworkPolicy,
	) {
		return
	}

	cidController := &Controller{
		logger:              p.Logger,
		clientset:           p.Clientset,
		namespace:           p.Namespace,
		pod:                 p.Pod,
		jobGroup:            p.JobGroup,
		metrics:             p.Metrics,
		ciliumIdentity:      p.CiliumIdentity,
		ciliumEndpoint:      p.CiliumEndpoint,
		ciliumEndpointSlice: p.CiliumEndpointSlice,
		oldNSSecurityLabels: make(map[string]labels.Labels),
		cesEnabled:          p.SharedCfg.EnableCiliumEndpointSlice,
		enqueueTimeTracker:  &EnqueueTimeTracker{clock: clock.RealClock{}, enqueuedAt: make(map[string]time.Time)},
	}

	cidController.initializeQueues()

	p.Lifecycle.Append(cidController)
}

func (c *Controller) Start(ctx cell.HookContext) error {
	c.logger.InfoContext(ctx, "Starting CID controller Operator")
	defer utilruntime.HandleCrash()

	// The Cilium Identity (CID) controller running in cilium-operator is
	// responsible only for managing CID API objects.
	//
	// Pod events are added to Pod work queue.
	// Namespace events are processed immediately and added to Pod work queue.
	// CID events are added to CID work queue.
	// Processing Pod work queue items are adding items to CID work queue.
	// Processed CID work queue items result in mutations to CID API objects.
	//
	// Diagram:
	//-------------------------Pod,CID event
	//---------------------------||
	//----------------------------V
	// Namespace event -> Resource work queue -> Mutate CID API objects
	c.jobGroup.Add(
		job.OneShot("op-managing-resource-wq", func(ctx context.Context, health cell.Health) error {
			if err := c.initReconciler(ctx); err != nil {
				return err
			}
			c.startEventProcessing()
			return c.runResourceWorker(ctx)
		}),
	)
	return nil
}

func (c *Controller) Stop(_ cell.HookContext) error {
	c.resourceQueue.ShutDown()

	return nil
}

func (c *Controller) initializeQueues() {

	c.logger.Info("CID controller resource work queue configuration",
		logfields.WorkQueueSyncBackOff, defaultSyncBackOff,
		logfields.WorkQueueMaxSyncBackOff, maxSyncBackOff)

	c.resourceQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemExponentialFailureRateLimiter[QueuedItem](defaultSyncBackOff, maxSyncBackOff),
		workqueue.TypedRateLimitingQueueConfig[QueuedItem]{Name: "ciliumidentity_resource"})
}

// startEventProcessing starts the event processing loop for the Controller.
// It processes events in the following order:
// 1. CiliumEndpointSlice, Pod, Namespace events concurrently
// 2. CiliumIdentity events
//
// The function uses a WaitGroup to ensure that each set of events is processed
// sequentially before moving on to the next.
func (c *Controller) startEventProcessing() {
	wg := &sync.WaitGroup{}

	wg.Add(3) // Adding delta for ces, pod, and ns events

	c.jobGroup.Add(
		job.OneShot("proc-ces-events", func(ctx context.Context, health cell.Health) error {
			return c.processCiliumEndpointSliceEvents(ctx, wg)
		}))
	c.jobGroup.Add(
		job.OneShot("proc-pod-events", func(ctx context.Context, health cell.Health) error {
			return c.processPodEvents(ctx, wg)
		}))
	c.jobGroup.Add(
		job.OneShot("proc-ns-events", func(ctx context.Context, health cell.Health) error {
			return c.processNamespaceEvents(ctx, wg)
		}))

	wg.Wait()

	wg.Add(1) // Adding cid events

	c.jobGroup.Add(
		job.OneShot("proc-cid-events", func(ctx context.Context, health cell.Health) error {
			return c.processCiliumIdentityEvents(ctx, wg)
		}))

	wg.Wait()
}

func (c *Controller) initReconciler(ctx context.Context) error {
	var err error
	c.reconciler, err = newReconciler(ctx, c.logger, c.clientset, c.namespace, c.pod, c.ciliumIdentity, c.ciliumEndpoint, c.ciliumEndpointSlice, c.cesEnabled, c)
	if err != nil {
		return fmt.Errorf("cid reconciler failed to init: %w", err)
	}
	c.logger.InfoContext(ctx, "Starting CID controller reconciler")
	return nil
}

func (c *Controller) runResourceWorker(ctx context.Context) error {
	c.logger.InfoContext(ctx, "Starting resource worker")
	defer c.logger.InfoContext(ctx, "Stopping resource worker")

	for c.processNextItem() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}

	return nil
}

func (c *Controller) enqueueReconciliation(item QueuedItem, delay time.Duration) {
	c.enqueueTimeTracker.Track(item.Key().String())
	c.resourceQueue.AddAfter(item, delay)
}

func (c *Controller) processNextItem() bool {
	item, quit := c.resourceQueue.Get()
	if quit {
		return false
	}
	defer c.resourceQueue.Done(item)
	processingStartTime := time.Now()
	enqueueTime, exists := c.enqueueTimeTracker.GetAndReset(item.Key().String())

	err := item.Reconcile(c.reconciler)
	if err != nil {
		retries := c.resourceQueue.NumRequeues(item)
		c.logger.Warn("Failed to process resource item",
			logfields.Key, item.Key(),
			logfields.Retries, retries,
			logfields.MaxRetries, maxProcessRetries,
			logfields.Error, err,
		)

		if retries < maxProcessRetries {
			c.enqueueTimeTracker.Track(item.Key().String())
			c.resourceQueue.AddRateLimited(item)
			return true
		}

		// Drop the pod from queue, exceeded max retries
		c.logger.Error("Dropping item from resource queue, exceeded maxRetries",
			logfields.Key, item.Key(),
			logfields.MaxRetries, maxProcessRetries,
			logfields.Error, err,
		)
	}

	if exists {
		enqueuedLatency := processingStartTime.Sub(enqueueTime).Seconds()
		processingLatency := time.Since(processingStartTime).Seconds()
		item.Meter(enqueuedLatency, processingLatency, err != nil, c.metrics)
	} else {
		c.logger.Warn("Enqueue time not found for queue item", logfields.Key, item.Key)
	}

	c.resourceQueue.Forget(item)
	return true
}
