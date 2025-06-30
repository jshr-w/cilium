// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package watchers

import (
	"context"
	"errors"
	"fmt"
	"sync"

	cilium_api_v2alpha1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/k8s/informer"
	"github.com/cilium/cilium/pkg/k8s/utils"
	"k8s.io/client-go/tools/cache"
)

var (
	errNoCES    = errors.New("object is not a *cilium_api_v2alpha1.CiliumEndpointSlice")
	cesIndexers = cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	}

	// CiliumEndpointStore contains all CiliumEndpoint present in k8s.
	// Warning: The CiliumEndpoints stored in the cache are not intended to be
	// used for Update operations in k8s as some of its fields were are not
	// populated.
	CiliumEndpointSliceStore cache.Indexer

	// CiliumEndpointsSynced is closed once the CiliumEndpointStore is synced
	// with k8s.
	CiliumEndpointSliceSynced = make(chan struct{})
	// once is used to make sure CiliumEndpointSliceInit is only setup once.
	onceS sync.Once
)

// CiliumEndpointSliceInit starts a CiliumEndpointSliceWatcher
func CiliumEndpointSliceInit(ctx context.Context, wg *sync.WaitGroup, clientset k8sClient.Clientset) {
	onceS.Do(func() {
		CiliumEndpointSliceStore = cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cesIndexers)

		ciliumEndpointSliceInformer := informer.NewInformerWithStore(
			utils.ListerWatcherFromTyped[*cilium_api_v2alpha1.CiliumEndpointSliceList](clientset.CiliumV2alpha1().CiliumEndpointSlices()),
			&cilium_api_v2alpha1.CiliumEndpointSlice{},
			0,
			cache.ResourceEventHandlerFuncs{},
			transformToCiliumEndpointSlice,
			CiliumEndpointSliceStore,
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			ciliumEndpointSliceInformer.Run(ctx.Done())
		}()

		cache.WaitForCacheSync(ctx.Done(), ciliumEndpointSliceInformer.HasSynced)
		close(CiliumEndpointSliceSynced)
	})
}

// TODO: Comment
// transformToCiliumEndpoint transforms a CiliumEndpoint to a minimal CiliumEndpoint
// containing only a minimal set of entities used to identity a CiliumEndpoint
// Warning: The CiliumEndpoints created by the converter are not intended to be
// used for Update operations in k8s. If the given obj can't be cast into either
// CiliumEndpoint nor DeletedFinalStateUnknown, an error is returned.
func transformToCiliumEndpointSlice(obj any) (any, error) {
	switch concreteObj := obj.(type) {
	case *cilium_api_v2alpha1.CiliumEndpointSlice:
		return obj, nil
	case cache.DeletedFinalStateUnknown:
		if _, ok := concreteObj.Obj.(*cilium_api_v2alpha1.CiliumEndpointSlice); ok {
			return obj, nil
		}
		return nil, fmt.Errorf("unknown object type %T", concreteObj.Obj)
		// TODO (This is handled much differently for CEP, will probably need to change)
	default:
		return nil, fmt.Errorf("unknown object type %T", concreteObj)
	}
}

// For now :)
// HasCE returns true or false if the Cilium Endpoint Slice store has the endpoint
// with the given name.
func HasCESWithPod(ns, name string) (bool, error) {
	if CiliumEndpointSliceStore == nil {
		return false, nil
	}

	for _, ces := range CiliumEndpointSliceStore.List() {
		if ces, ok := ces.(*cilium_api_v2alpha1.CiliumEndpointSlice); ok {
			if ces.Namespace != ns {
				continue
			}
			for _, cep := range ces.Endpoints {
				if cep.Name == name {
					return true, nil
				}
			}
		}
		return false, fmt.Errorf("%w - found %T", errNoCES, ces)

	}
	return false, fmt.Errorf("no CES found for pod %s/%s", ns, name)
}
