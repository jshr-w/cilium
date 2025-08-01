// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ciliumidentity

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sTesting "k8s.io/client-go/testing"

	"github.com/cilium/cilium/operator/k8s"
	cidtest "github.com/cilium/cilium/operator/pkg/ciliumidentity/testutils"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/identity/key"
	capi_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	capi_v2a1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client/testutils"
	"github.com/cilium/cilium/pkg/k8s/resource"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	k8sUtils "github.com/cilium/cilium/pkg/k8s/utils"
	"github.com/cilium/cilium/pkg/labels"
)

var (
	testLbsA = map[string]string{"key-a": "val-1"}
	testLbsB = map[string]string{"key-b": "val-2"}
	testLbsC = map[string]string{"key-c": "val-3"}
)

type testQueueOps struct {
	fakeWorkQueue map[string]bool
}

func (t testQueueOps) enqueueReconciliation(item QueuedItem, delay time.Duration) {
	t.fakeWorkQueue[item.Key().String()] = true
}

func testNewReconciler(t *testing.T, ctx context.Context, enableCES bool) (*reconciler, *testQueueOps, *k8sClient.FakeClientset, func()) {
	var namespace resource.Resource[*slim_corev1.Namespace]
	var pod resource.Resource[*slim_corev1.Pod]
	var ciliumIdentity resource.Resource[*capi_v2.CiliumIdentity]
	var ciliumEndpoint resource.Resource[*capi_v2.CiliumEndpoint]
	var ciliumEndpointSlice resource.Resource[*capi_v2a1.CiliumEndpointSlice]
	var fakeClient *k8sClient.FakeClientset

	h := hive.New(
		k8sClient.FakeClientCell(),
		k8s.ResourcesCell,
		cell.Invoke(func(
			c *k8sClient.FakeClientset,
			nsResource resource.Resource[*slim_corev1.Namespace],
			podResource resource.Resource[*slim_corev1.Pod],
			cidResource resource.Resource[*capi_v2.CiliumIdentity],
			cepResource resource.Resource[*capi_v2.CiliumEndpoint],
			cesResource resource.Resource[*capi_v2a1.CiliumEndpointSlice],
		) error {
			fakeClient = c
			namespace = nsResource
			pod = podResource
			ciliumIdentity = cidResource
			ciliumEndpoint = cepResource
			ciliumEndpointSlice = cesResource
			return nil
		}),
	)
	tlog := hivetest.Logger(t)
	_ = h.Start(tlog, ctx)
	cleanupFunc := func() {
		_ = h.Stop(tlog, ctx)
	}

	queueOps := &testQueueOps{fakeWorkQueue: make(map[string]bool)}
	reconciler, _ := newReconciler(
		ctx,
		tlog,
		fakeClient.Clientset,
		namespace,
		pod,
		ciliumIdentity,
		ciliumEndpoint,
		ciliumEndpointSlice,
		enableCES,
		queueOps,
	)
	return reconciler, queueOps, fakeClient, cleanupFunc
}

func testVerifyCIDUsageInPods(t *testing.T, usage *CIDUsageInPods, expectedCIDs, expectedPods int) {
	if expectedCIDs != len(usage.cidUsageCount) {
		t.Errorf("Unexpected number of unique CIDs. Expected: %d, Got: %d", expectedCIDs, len(usage.cidUsageCount))
	}
	if expectedPods != len(usage.podToCID) {
		t.Errorf("Unexpected number of pods. Expected: %d, Got: %d", expectedPods, len(usage.podToCID))
	}

	totalUsedCIDs := 0
	for _, count := range usage.cidUsageCount {
		totalUsedCIDs += count
	}

	if expectedPods != totalUsedCIDs {
		t.Errorf("Total CID usage does not match expected pod count. Expected: %d, Got: %d", expectedPods, totalUsedCIDs)
	}
}

func TestReconcileCID(t *testing.T) {
	cidName := "1000"

	testCases := []struct {
		name                string
		cidKey              resource.Key
		initialStoreState   *capi_v2.CiliumIdentity // CID initially in the store
		desiredState        map[string]string       // Desired CID labels
		usedInPods          bool
		errorDuringDeletion bool
		expectedCreate      *capi_v2.CiliumIdentity
		expectedUpdate      *capi_v2.CiliumIdentity
		expectedDelete      string // Name of the CID to be deleted
		expectedQueueSize   int
	}{
		{
			name:              "default",
			cidKey:            cidResourceKey(cidName),
			initialStoreState: cidtest.NewCID(cidName, testLbsA),
			usedInPods:        false,
			expectedQueueSize: 0, // TODO queue size should be 1 if OP deletes CID
		},
		{
			name:           "cid_only_in_desired",
			cidKey:         cidResourceKey(cidName),
			desiredState:   testLbsA,
			usedInPods:     true,
			expectedCreate: cidtest.NewCID(cidName, testLbsA), // CID is created
		},
		{
			name:              "cid_in_desired_and_store",
			cidKey:            cidResourceKey(cidName),
			initialStoreState: cidtest.NewCID(cidName, testLbsA),
			desiredState:      testLbsA,
			usedInPods:        true,
		},
		{
			name:              "cid_store_different_labels_than_desired",
			cidKey:            cidResourceKey(cidName),
			initialStoreState: cidtest.NewCID(cidName, testLbsB),
			desiredState:      testLbsA,
			usedInPods:        true,
			expectedUpdate:    cidtest.NewCID(cidName, testLbsA),
		},
		{
			name:   "cid_not_in_store",
			cidKey: cidResourceKey(cidName),
		},
		{
			name:              "cid_used_in_pods_no_desired",
			cidKey:            cidResourceKey(cidName),
			initialStoreState: cidtest.NewCID(cidName, testLbsA),
			usedInPods:        true,
		},
		{
			name:              "cid_in_store_not_used",
			cidKey:            cidResourceKey(cidName),
			initialStoreState: cidtest.NewCID(cidName, testLbsA),
			usedInPods:        false,
			desiredState:      testLbsA,
			expectedQueueSize: 0, // TODO queue size should be 1 if OP deletes CID
		},
		{
			name:         "cid_in_desired_not_used",
			cidKey:       cidResourceKey(cidName),
			usedInPods:   false,
			desiredState: testLbsA,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reconciler, queueOps, _, cleanupFunc := testNewReconciler(t, ctx, false)
			defer cleanupFunc()

			cs := reconciler.clientset.(*k8sClient.FakeClientset)
			var createCID, updateCID *capi_v2.CiliumIdentity
			var deleteCIDName string
			cs.CiliumFakeClientset.PrependReactor("create", "*", func(action k8sTesting.Action) (handled bool, ret runtime.Object, err error) {
				pa := action.(k8sTesting.CreateAction)
				createCID = pa.GetObject().(*capi_v2.CiliumIdentity)
				return true, nil, nil
			})
			cs.CiliumFakeClientset.PrependReactor("update", "*", func(action k8sTesting.Action) (handled bool, ret runtime.Object, err error) {
				pa := action.(k8sTesting.UpdateAction)
				updateCID = pa.GetObject().(*capi_v2.CiliumIdentity)
				return true, nil, nil
			})
			cs.CiliumFakeClientset.PrependReactor("delete", "*", func(action k8sTesting.Action) (handled bool, ret runtime.Object, err error) {
				if tc.errorDuringDeletion {
					return false, nil, fmt.Errorf("forced error on deletion")
				}
				pa := action.(k8sTesting.DeleteAction)
				deleteCIDName = pa.GetName()
				return true, nil, nil
			})

			if tc.initialStoreState != nil {
				if err := reconciler.cidStore.CacheStore().Add(tc.initialStoreState); err != nil {
					t.Errorf("Error adding CID to cache store: %v", err)
				}
			}

			if tc.desiredState != nil {
				if err := reconciler.upsertDesiredState(tc.cidKey.Name, key.GetCIDKeyFromLabels(tc.desiredState, labels.LabelSourceK8s)); err != nil {
					t.Errorf("Unexpected error upserting desired state: %v", err)
				}
			}

			if tc.usedInPods {
				reconciler.cidUsageInPods.cidUsageCount[tc.cidKey.Name] = 1
			}

			if err := reconciler.reconcileCID(tc.cidKey); err != nil {
				t.Errorf("Unexpected error during reconciliation: %v", err)
			}

			if !reflect.DeepEqual(createCID, tc.expectedCreate) {
				t.Errorf("Unexpected createCID result: got %v, want %v", createCID, tc.expectedCreate)
			}
			if !reflect.DeepEqual(updateCID, tc.expectedUpdate) {
				t.Errorf("Unexpected updateCID result: got %v, want %v", updateCID, tc.expectedUpdate)
			}
			if deleteCIDName != tc.expectedDelete {
				t.Errorf("Unexpected deleteCIDName result: got %v, want %v", deleteCIDName, tc.expectedDelete)
			}
			if len(queueOps.fakeWorkQueue) != tc.expectedQueueSize {
				t.Errorf("Unexpected fakeWorkQueue size: got %v, want %v", len(queueOps.fakeWorkQueue), tc.expectedQueueSize)
			}

			// TODO check for CID deletion too, adapt once we decide how to handle CID deletion
			// See https://github.com/cilium/cilium/pull/33380
		})
	}
}

func TestReconcilePod(t *testing.T) {
	ns1 := cidtest.NewNamespace("ns1", nil)

	pod1 := cidtest.NewPod("pod1", ns1.Name, testLbsA, "node1")
	pod2 := cidtest.NewPod("pod2", ns1.Name, testLbsA, "node1")

	cidName := "1000"

	type expectedState struct {
		queueSize           int
		existsInQueue       []string
		numDesiredCIDLabels int
		expectedCIDs        int
		expectedPods        int
		expectedCID         string
	}

	testCases := []struct {
		name            string
		newPod          *slim_corev1.Pod
		existingPods    []*slim_corev1.Pod
		existingCIDs    []*capi_v2.CiliumIdentity
		initialPodToCID map[resource.Key]string
		expected        expectedState
		expectError     bool
	}{
		{
			name:     "pod_not_in_store_or_mapping",
			newPod:   pod1,
			expected: expectedState{},
		},
		{
			name:         "new_pod_with_no_cid",
			newPod:       pod1,
			existingPods: []*slim_corev1.Pod{pod1},
			expected: expectedState{
				queueSize:           1,
				numDesiredCIDLabels: 1,
				expectedPods:        1,
				expectedCIDs:        1,
			},
		},
		{
			name:         "pod_in_store_with_cid_not_in_mapping",
			newPod:       pod1,
			existingPods: []*slim_corev1.Pod{pod1},
			existingCIDs: []*capi_v2.CiliumIdentity{cidtest.NewCIDWithNamespace(cidName, pod1, ns1)},
			expected: expectedState{
				queueSize:           0,
				numDesiredCIDLabels: 1,
				expectedPods:        1,
				expectedCIDs:        1,
				expectedCID:         cidName,
			},
		},
		{
			name:            "pod_in_store_with_cid_and_mapping",
			newPod:          pod1,
			existingPods:    []*slim_corev1.Pod{pod1},
			existingCIDs:    []*capi_v2.CiliumIdentity{cidtest.NewCIDWithNamespace(cidName, pod1, ns1)},
			initialPodToCID: map[resource.Key]string{podResourceKey(pod1.Name, ns1.Name): cidName},
			expected: expectedState{
				queueSize:           0,
				numDesiredCIDLabels: 1,
				expectedPods:        1,
				expectedCIDs:        1,
				expectedCID:         cidName,
			},
		},
		{
			name:            "multiple_pods_same_cid",
			newPod:          pod2,
			existingPods:    []*slim_corev1.Pod{pod1, pod2},
			existingCIDs:    []*capi_v2.CiliumIdentity{cidtest.NewCIDWithNamespace(cidName, pod1, ns1)},
			initialPodToCID: map[resource.Key]string{podResourceKey(pod1.Name, ns1.Name): cidName},
			expected: expectedState{
				queueSize:           0,
				numDesiredCIDLabels: 1,
				expectedPods:        2,
				expectedCIDs:        1,
				expectedCID:         cidName,
			},
		},
		{
			name:            "pod_with_different_labels",
			newPod:          cidtest.NewPod("pod1", ns1.Name, testLbsB, "node1"),
			existingPods:    []*slim_corev1.Pod{cidtest.NewPod("pod1", ns1.Name, testLbsB, "node1")},
			existingCIDs:    []*capi_v2.CiliumIdentity{cidtest.NewCIDWithNamespace(cidName, pod1, ns1)},
			initialPodToCID: map[resource.Key]string{podResourceKey(pod1.Name, ns1.Name): cidName},
			expected: expectedState{
				queueSize:           1, // TODO queue size should be 2 if OP deletes CID
				numDesiredCIDLabels: 1,
				expectedPods:        1,
				expectedCIDs:        1,
			},
		},
		{
			name:            "pod_in_store_with_cid_and_different_mapping",
			newPod:          pod1,
			existingPods:    []*slim_corev1.Pod{pod1},
			existingCIDs:    []*capi_v2.CiliumIdentity{cidtest.NewCIDWithNamespace(cidName, pod1, ns1)},
			initialPodToCID: map[resource.Key]string{podResourceKey(pod1.Name, ns1.Name): "2000"},
			expected: expectedState{
				queueSize:           0, // TODO queue size should be 1 if OP deletes CID
				numDesiredCIDLabels: 1,
				expectedPods:        1,
				expectedCIDs:        1,
				expectedCID:         cidName,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reconciler, queueOps, _, cleanupFunc := testNewReconciler(t, ctx, false)
			defer cleanupFunc()

			for _, pod := range tc.existingPods {
				_ = reconciler.nsStore.CacheStore().Add(cidtest.NewNamespace(pod.GetNamespace(), nil))
				_ = reconciler.podStore.CacheStore().Add(pod)
			}
			for _, cid := range tc.existingCIDs {
				_ = reconciler.cidStore.CacheStore().Add(cid)
			}
			for podKey, cidName := range tc.initialPodToCID {
				reconciler.cidUsageInPods.AssignCIDToPod(podKey.String(), cidName)
			}

			if err := reconciler.reconcilePod(podResourceKey(tc.newPod.Name, tc.newPod.Namespace)); err != nil {
				t.Errorf("Unexpected error behavior: got error %v", err)
			}

			if tc.expected.queueSize != len(queueOps.fakeWorkQueue) {
				t.Errorf("Queue size mismatch: expected %d, got %d", tc.expected.queueSize, len(queueOps.fakeWorkQueue))
			}

			for _, k := range tc.expected.existsInQueue {
				if _, exists := queueOps.fakeWorkQueue[k]; !exists {
					t.Errorf("Item missing from fake work queue: expected key '%s'", k)
				}
			}
			if tc.expected.numDesiredCIDLabels != len(reconciler.desiredCIDState.idToLabels) {
				t.Errorf("Number of desired CID labels mismatch: expected %d, got %d", tc.expected.numDesiredCIDLabels, len(reconciler.desiredCIDState.idToLabels))
			}

			testVerifyCIDUsageInPods(t, reconciler.cidUsageInPods, tc.expected.expectedCIDs, tc.expected.expectedPods)

			if len(tc.expected.expectedCID) > 0 {
				lbs, exists := reconciler.desiredCIDState.LookupByID(cidName)
				if !exists {
					t.Errorf("Expected CID %s not found in desiredCIDState", cidName)
				}

				expectedLbs := key.GetCIDKeyFromLabels(
					k8sUtils.SanitizePodLabels(tc.newPod.ObjectMeta.Labels, ns1, "", ""),
					labels.LabelSourceK8s,
				).GetAsMap()

				if diff := cmp.Diff(lbs.GetAsMap(), expectedLbs); diff != "" {
					t.Errorf("Labels differ:\n%s", diff)
				}
			}

		})
	}
}

func TestReconcileNS(t *testing.T) {
	ns1 := cidtest.NewNamespace("ns1", nil)

	pod1 := cidtest.NewPod("pod1", ns1.Name, testLbsA, "node1")
	pod2 := cidtest.NewPod("pod2", ns1.Name, testLbsA, "node1")

	testCases := []struct {
		name              string
		nsName            string
		pods              []*slim_corev1.Pod
		expectedQueueSize int
	}{{
		name:              "empty_ns",
		nsName:            "ns1",
		pods:              []*slim_corev1.Pod{},
		expectedQueueSize: 0,
	},
		{
			name:   "unrelated_ns",
			nsName: "ns2",
			pods: []*slim_corev1.Pod{
				pod1,
			},
			expectedQueueSize: 0,
		},
		{
			name:   "multiple_pods",
			nsName: ns1.Name,
			pods: []*slim_corev1.Pod{
				pod1,
				pod2,
			},
			expectedQueueSize: 2,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reconciler, queueOps, _, cleanupFunc := testNewReconciler(t, ctx, false)
			defer cleanupFunc()

			if err := reconciler.nsStore.CacheStore().Add(cidtest.NewNamespace(tc.nsName, nil)); err != nil {
				t.Errorf("Unexpected error behavior: got error %v", err)
			}

			for _, pod := range tc.pods {
				if err := reconciler.podStore.CacheStore().Add(pod); err != nil {
					t.Errorf("Unexpected error behavior: got error %v", err)
				}
			}

			err := reconciler.reconcileNamespace(nsResourceKey(tc.nsName))

			if err != nil {
				t.Errorf("Unexpected error behavior: got error %v", err)
			}
			if tc.expectedQueueSize != len(queueOps.fakeWorkQueue) {
				t.Errorf("Expected queue size %d, but got %d", tc.expectedQueueSize, len(queueOps.fakeWorkQueue))
			}
		})
	}
}

func TestHandleStoreCIDMatch(t *testing.T) {
	testCases := []struct {
		name        string
		cidList     []*capi_v2.CiliumIdentity
		expectedCID string
	}{
		{
			name: "one_item",
			cidList: []*capi_v2.CiliumIdentity{
				{ObjectMeta: metav1.ObjectMeta{Name: "1000"}},
			},
			expectedCID: "1000",
		},
		{
			name: "three_items",
			cidList: []*capi_v2.CiliumIdentity{
				{ObjectMeta: metav1.ObjectMeta{Name: "1000"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "2000"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "3000"}},
			},
			expectedCID: "1000",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reconciler, _, _, cleanupFunc := testNewReconciler(t, ctx, false)
			defer cleanupFunc()

			cid, err := reconciler.handleStoreCIDMatch(tc.cidList)

			if err != nil {
				t.Errorf("Expected no error, got: %v", err)
			}

			if tc.expectedCID != cid {
				t.Errorf("Expected CID %v, but got %v", tc.expectedCID, cid)
			}
		})
	}
}
