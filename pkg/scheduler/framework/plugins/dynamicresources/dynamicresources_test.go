/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dynamicresources

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	resourcev1alpha2 "k8s.io/api/resource/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	cgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

var (
	podKind = v1.SchemeGroupVersion.WithKind("Pod")

	podName       = "my-pod"
	podUID        = "1234"
	resourceName  = "my-resource"
	resourceName2 = resourceName + "-2"
	claimName     = podName + "-" + resourceName
	claimName2    = podName + "-" + resourceName + "-2"
	className     = "my-resource-class"
	namespace     = "default"

	resourceClass = &resourcev1alpha2.ResourceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
		DriverName: "some-driver",
	}
	structuredResourceClass = &resourcev1alpha2.ResourceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
		DriverName:           "some-driver",
		StructuredParameters: ptr.To(true),
	}
	structuredResourceClassWithParams = &resourcev1alpha2.ResourceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
		DriverName:           "some-driver",
		StructuredParameters: ptr.To(true),
		ParametersRef: &resourcev1alpha2.ResourceClassParametersReference{
			Name:      className,
			Namespace: namespace,
			Kind:      "ResourceClassParameters",
			APIGroup:  "resource.k8s.io",
		},
	}
	structuredResourceClassWithCRD = &resourcev1alpha2.ResourceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
		DriverName:           "some-driver",
		StructuredParameters: ptr.To(true),
		ParametersRef: &resourcev1alpha2.ResourceClassParametersReference{
			Name:      className,
			Namespace: namespace,
			Kind:      "ResourceClassParameters",
			APIGroup:  "example.com",
		},
	}

	podWithClaimName = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, Source: v1.ClaimSource{ResourceClaimName: &claimName}}).
				Obj()
	otherPodWithClaimName = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID + "-II").
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, Source: v1.ClaimSource{ResourceClaimName: &claimName}}).
				Obj()
	podWithClaimTemplate = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, Source: v1.ClaimSource{ResourceClaimTemplateName: &claimName}}).
				Obj()
	podWithClaimTemplateInStatus = func() *v1.Pod {
		pod := podWithClaimTemplate.DeepCopy()
		pod.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{
			{
				Name:              pod.Spec.ResourceClaims[0].Name,
				ResourceClaimName: &claimName,
			},
		}
		return pod
	}()
	podWithTwoClaimNames = st.MakePod().Name(podName).Namespace(namespace).
				UID(podUID).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName, Source: v1.ClaimSource{ResourceClaimName: &claimName}}).
				PodResourceClaims(v1.PodResourceClaim{Name: resourceName2, Source: v1.ClaimSource{ResourceClaimName: &claimName2}}).
				Obj()

	workerNode      = &st.MakeNode().Name("worker").Label("kubernetes.io/hostname", "worker").Node
	workerNodeSlice = st.MakeResourceSlice("worker", "some-driver").NamedResourcesInstances("instance-1").Obj()

	claimParameters = st.MakeClaimParameters().Name(claimName).Namespace(namespace).
			NamedResourcesRequests("some-driver", "true").
			Shareable(true).
			GeneratedFrom(&resourcev1alpha2.ResourceClaimParametersReference{
			Name:     claimName,
			Kind:     "ResourceClaimParameters",
			APIGroup: "example.com",
		}).
		Obj()
	claimParametersOtherNamespace = st.MakeClaimParameters().Name(claimName).Namespace(namespace+"-2").
					NamedResourcesRequests("some-driver", "true").
					Shareable(true).
					GeneratedFrom(&resourcev1alpha2.ResourceClaimParametersReference{
			Name:     claimName,
			Kind:     "ResourceClaimParameters",
			APIGroup: "example.com",
		}).
		Obj()
	classParameters = st.MakeClassParameters().Name(className).Namespace(namespace).
			NamedResourcesFilters("some-driver", "true").
			GeneratedFrom(&resourcev1alpha2.ResourceClassParametersReference{
			Name:      className,
			Namespace: namespace,
			Kind:      "ResourceClassParameters",
			APIGroup:  "example.com",
		}).
		Obj()

	claim = st.MakeResourceClaim().
		Name(claimName).
		Namespace(namespace).
		ResourceClassName(className).
		Obj()
	pendingImmediateClaim = st.FromResourceClaim(claim).
				AllocationMode(resourcev1alpha2.AllocationModeImmediate).
				Obj()
	structuredAllocatedImmediateClaim = st.FromResourceClaim(pendingImmediateClaim).
						Allocation("some-driver", &resourcev1alpha2.AllocationResult{}).
						Structured("worker", "instance-1").
						Obj()
	pendingDelayedClaim = st.FromResourceClaim(claim).
				OwnerReference(podName, podUID, podKind).
				AllocationMode(resourcev1alpha2.AllocationModeWaitForFirstConsumer).
				Obj()
	pendingDelayedClaim2 = st.FromResourceClaim(pendingDelayedClaim).
				Name(claimName2).
				Obj()
	deallocatingClaim = st.FromResourceClaim(pendingImmediateClaim).
				Allocation("some-driver", &resourcev1alpha2.AllocationResult{}).
				DeallocationRequested(true).
				Obj()
	inUseClaim = st.FromResourceClaim(pendingImmediateClaim).
			Allocation("some-driver", &resourcev1alpha2.AllocationResult{}).
			ReservedForPod(podName, types.UID(podUID)).
			Obj()
	structuredInUseClaim = st.FromResourceClaim(inUseClaim).
				Structured("worker", "instance-1").
				Obj()
	allocatedClaim = st.FromResourceClaim(pendingDelayedClaim).
			Allocation("some-driver", &resourcev1alpha2.AllocationResult{}).
			Obj()

	pendingDelayedClaimWithParams      = st.FromResourceClaim(pendingDelayedClaim).ParametersRef(claimName).Obj()
	structuredAllocatedClaim           = st.FromResourceClaim(allocatedClaim).Structured("worker", "instance-1").Obj()
	structuredAllocatedClaimWithParams = st.FromResourceClaim(structuredAllocatedClaim).ParametersRef(claimName).Obj()

	otherStructuredAllocatedClaim = st.FromResourceClaim(structuredAllocatedClaim).Name(structuredAllocatedClaim.Name + "-other").Obj()

	allocatedDelayedClaimWithWrongTopology = st.FromResourceClaim(allocatedClaim).
						Allocation("some-driver", &resourcev1alpha2.AllocationResult{AvailableOnNodes: st.MakeNodeSelector().In("no-such-label", []string{"no-such-value"}).Obj()}).
						Obj()
	structuredAllocatedDelayedClaimWithWrongTopology = st.FromResourceClaim(allocatedDelayedClaimWithWrongTopology).
								Structured("worker-2", "instance-1").
								Obj()
	allocatedImmediateClaimWithWrongTopology = st.FromResourceClaim(allocatedDelayedClaimWithWrongTopology).
							AllocationMode(resourcev1alpha2.AllocationModeImmediate).
							Obj()
	structuredAllocatedImmediateClaimWithWrongTopology = st.FromResourceClaim(allocatedImmediateClaimWithWrongTopology).
								Structured("worker-2", "instance-1").
								Obj()
	allocatedClaimWithGoodTopology = st.FromResourceClaim(allocatedClaim).
					Allocation("some-driver", &resourcev1alpha2.AllocationResult{AvailableOnNodes: st.MakeNodeSelector().In("kubernetes.io/hostname", []string{"worker"}).Obj()}).
					Obj()
	structuredAllocatedClaimWithGoodTopology = st.FromResourceClaim(allocatedClaimWithGoodTopology).
							Structured("worker", "instance-1").
							Obj()
	otherClaim = st.MakeResourceClaim().
			Name("not-my-claim").
			Namespace(namespace).
			ResourceClassName(className).
			Obj()

	scheduling = st.MakePodSchedulingContexts().Name(podName).Namespace(namespace).
			OwnerReference(podName, podUID, podKind).
			Obj()
	schedulingPotential = st.FromPodSchedulingContexts(scheduling).
				PotentialNodes(workerNode.Name).
				Obj()
	schedulingSelectedPotential = st.FromPodSchedulingContexts(schedulingPotential).
					SelectedNode(workerNode.Name).
					Obj()
	schedulingInfo = st.FromPodSchedulingContexts(schedulingPotential).
			ResourceClaims(resourcev1alpha2.ResourceClaimSchedulingStatus{Name: resourceName},
			resourcev1alpha2.ResourceClaimSchedulingStatus{Name: resourceName2}).
		Obj()
)

func reserve(claim *resourcev1alpha2.ResourceClaim, pod *v1.Pod) *resourcev1alpha2.ResourceClaim {
	return st.FromResourceClaim(claim).
		ReservedForPod(pod.Name, types.UID(pod.UID)).
		Obj()
}

// claimWithCRD replaces the in-tree group with "example.com".
func claimWithCRD(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
	claim = claim.DeepCopy()
	claim.Spec.ParametersRef.APIGroup = "example.com"
	return claim
}

// classWithCRD replaces the in-tree group with "example.com".
func classWithCRD(class *resourcev1alpha2.ResourceClass) *resourcev1alpha2.ResourceClass {
	class = class.DeepCopy()
	class.ParametersRef.APIGroup = "example.com"
	return class
}

func breakCELInClaimParameters(parameters *resourcev1alpha2.ResourceClaimParameters) *resourcev1alpha2.ResourceClaimParameters {
	parameters = parameters.DeepCopy()
	for i := range parameters.DriverRequests {
		for e := range parameters.DriverRequests[i].Requests {
			parameters.DriverRequests[i].Requests[e].NamedResources.Selector = `attributes.bool["no-such-attribute"]`
		}
	}
	return parameters
}

func breakCELInClassParameters(parameters *resourcev1alpha2.ResourceClassParameters) *resourcev1alpha2.ResourceClassParameters {
	parameters = parameters.DeepCopy()
	for i := range parameters.Filters {
		parameters.Filters[i].NamedResources.Selector = `attributes.bool["no-such-attribute"]`
	}
	return parameters
}

// result defines the expected outcome of some operation. It covers
// operation's status and the state of the world (= objects).
type result struct {
	status *framework.Status
	// changes contains a mapping of name to an update function for
	// the corresponding object. These functions apply exactly the expected
	// changes to a copy of the object as it existed before the operation.
	changes change

	// added contains objects created by the operation.
	added []metav1.Object

	// removed contains objects deleted by the operation.
	removed []metav1.Object

	// assumedClaim is the one claim which is expected to be assumed,
	// nil if none.
	assumedClaim *resourcev1alpha2.ResourceClaim

	// inFlightClaim is the one claim which is expected to be tracked as
	// in flight, nil if none.
	inFlightClaim *resourcev1alpha2.ResourceClaim
}

// change contains functions for modifying objects of a certain type. These
// functions will get called for all objects of that type. If they needs to
// make changes only to a particular instance, then it must check the name.
type change struct {
	scheduling func(*resourcev1alpha2.PodSchedulingContext) *resourcev1alpha2.PodSchedulingContext
	claim      func(*resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim
}
type perNodeResult map[string]result

func (p perNodeResult) forNode(nodeName string) result {
	if p == nil {
		return result{}
	}
	return p[nodeName]
}

type want struct {
	preenqueue       result
	preFilterResult  *framework.PreFilterResult
	prefilter        result
	filter           perNodeResult
	prescore         result
	reserve          result
	unreserve        result
	prebind          result
	postbind         result
	postFilterResult *framework.PostFilterResult
	postfilter       result

	// unreserveAfterBindFailure, if set, triggers a call to Unreserve
	// after PreBind, as if the actual Bind had failed.
	unreserveAfterBindFailure *result

	// unreserveBeforePreBind, if set, triggers a call to Unreserve
	// before PreBind, as if the some other PreBind plugin had failed.
	unreserveBeforePreBind *result
}

// prepare contains changes for objects in the API server.
// Those changes are applied before running the steps. This can
// be used to simulate concurrent changes by some other entities
// like a resource driver.
type prepare struct {
	filter     change
	prescore   change
	reserve    change
	unreserve  change
	prebind    change
	postbind   change
	postfilter change
}

func TestPlugin(t *testing.T) {
	testcases := map[string]struct {
		nodes       []*v1.Node // default if unset is workerNode
		pod         *v1.Pod
		claims      []*resourcev1alpha2.ResourceClaim
		classes     []*resourcev1alpha2.ResourceClass
		schedulings []*resourcev1alpha2.PodSchedulingContext

		// objs get stored directly in the fake client, without passing
		// through reactors, in contrast to the types above.
		objs []apiruntime.Object

		prepare prepare
		want    want
		disable bool
	}{
		"empty": {
			pod: st.MakePod().Name("foo").Namespace("default").Obj(),
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.Skip),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"claim-reference": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedClaim, otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"claim-reference-structured": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{structuredAllocatedClaim, otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"claim-template": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedClaim, otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"claim-template-structured": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourcev1alpha2.ResourceClaim{structuredAllocatedClaim, otherClaim},
			want: want{
				prebind: result{
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status.ReservedFor = inUseClaim.Status.ReservedFor
							}
							return claim
						},
					},
				},
			},
		},
		"missing-claim": {
			pod:    podWithClaimTemplate, // status not set
			claims: []*resourcev1alpha2.ResourceClaim{allocatedClaim, otherClaim},
			want: want{
				preenqueue: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `pod "default/my-pod": ResourceClaim not created yet`),
				},
			},
		},
		"deleted-claim": {
			pod: podWithClaimTemplateInStatus,
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				return []*resourcev1alpha2.ResourceClaim{claim}
			}(),
			want: want{
				preenqueue: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim "my-pod-my-resource" is being deleted`),
				},
			},
		},
		"wrong-claim": {
			pod: podWithClaimTemplateInStatus,
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.OwnerReferences[0].UID += "123"
				return []*resourcev1alpha2.ResourceClaim{claim}
			}(),
			want: want{
				preenqueue: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `ResourceClaim default/my-pod-my-resource was not created for pod default/my-pod (pod is not owner)`),
				},
			},
		},
		"waiting-for-immediate-allocation": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingImmediateClaim},
			classes: []*resourcev1alpha2.ResourceClass{resourceClass},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `unallocated immediate resourceclaim`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"immediate-allocation-structured-no-resources": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingImmediateClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim cannot be allocated for the node (unsuitable)`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},
		"immediate-allocation-structured-with-resources": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingImmediateClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedImmediateClaim,
				},
				prebind: result{
					assumedClaim: reserve(structuredAllocatedImmediateClaim, podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredAllocatedImmediateClaim.Finalizers
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredAllocatedImmediateClaim, podWithClaimName),
				},
			},
		},
		"delayed-allocation-structured-no-resources": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim cannot be allocated for the node (unsuitable)`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},
		"delayed-allocation-structured-with-resources": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedClaim,
				},
				prebind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredAllocatedClaim.Finalizers
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
				},
			},
		},
		"delayed-allocation-structured-with-resources-has-finalizer": {
			// As before. but the finalizer is already set. Could happen if
			// the scheduler got interrupted.
			pod: podWithClaimName,
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claim := pendingDelayedClaim.DeepCopy()
				claim.Finalizers = structuredAllocatedClaim.Finalizers
				return []*resourcev1alpha2.ResourceClaim{claim}
			}(),
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedClaim,
				},
				prebind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
				},
			},
		},
		"delayed-allocation-structured-with-resources-finalizer-gets-removed": {
			// As before. but the finalizer is already set. Then it gets
			// removed before the scheduler reaches PreBind.
			pod: podWithClaimName,
			claims: func() []*resourcev1alpha2.ResourceClaim {
				claim := pendingDelayedClaim.DeepCopy()
				claim.Finalizers = structuredAllocatedClaim.Finalizers
				return []*resourcev1alpha2.ResourceClaim{claim}
			}(),
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			prepare: prepare{
				prebind: change{
					claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
						claim.Finalizers = nil
						return claim
					},
				},
			},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedClaim,
				},
				prebind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredAllocatedClaim.Finalizers
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
				},
			},
		},
		"delayed-allocation-structured-with-resources-finalizer-gets-added": {
			// No finalizer initially, then it gets added before
			// the scheduler reaches PreBind. Shouldn't happen?
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			prepare: prepare{
				prebind: change{
					claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
						claim.Finalizers = structuredAllocatedClaim.Finalizers
						return claim
					},
				},
			},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedClaim,
				},
				prebind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredAllocatedClaim, podWithClaimName),
				},
			},
		},
		"delayed-allocation-structured-skip-bind": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedClaim,
				},
				unreserveBeforePreBind: &result{},
			},
		},
		"delayed-allocation-structured-exhausted-resources": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim, otherStructuredAllocatedClaim},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClass},
			objs:    []apiruntime.Object{workerNodeSlice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim cannot be allocated for the node (unsuitable)`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},

		"with-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaimWithParams},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClassWithParams},
			objs:    []apiruntime.Object{claimParameters, classParameters, workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: structuredAllocatedClaimWithParams,
				},
				prebind: result{
					assumedClaim: reserve(structuredAllocatedClaimWithParams, podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredAllocatedClaim.Finalizers
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(structuredAllocatedClaimWithParams, podWithClaimName),
				},
			},
		},

		"with-translated-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{claimWithCRD(pendingDelayedClaimWithParams)},
			classes: []*resourcev1alpha2.ResourceClass{classWithCRD(structuredResourceClassWithCRD)},
			objs:    []apiruntime.Object{claimParameters, claimParametersOtherNamespace /* must be ignored */, classParameters, workerNodeSlice},
			want: want{
				reserve: result{
					inFlightClaim: claimWithCRD(structuredAllocatedClaimWithParams),
				},
				prebind: result{
					assumedClaim: reserve(claimWithCRD(structuredAllocatedClaimWithParams), podWithClaimName),
					changes: change{
						claim: func(claim *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							if claim.Name == claimName {
								claim = claim.DeepCopy()
								claim.Finalizers = structuredAllocatedClaim.Finalizers
								claim.Status = structuredInUseClaim.Status
							}
							return claim
						},
					},
				},
				postbind: result{
					assumedClaim: reserve(claimWithCRD(structuredAllocatedClaimWithParams), podWithClaimName),
				},
			},
		},

		"missing-class-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaimWithParams},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClassWithParams},
			objs:    []apiruntime.Object{claimParameters, workerNodeSlice},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `class parameters default/my-resource-class not found`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},

		"missing-claim-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaimWithParams},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClassWithParams},
			objs:    []apiruntime.Object{classParameters, workerNodeSlice},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `claim parameters default/my-pod-my-resource not found`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},

		"missing-translated-class-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{claimWithCRD(pendingDelayedClaimWithParams)},
			classes: []*resourcev1alpha2.ResourceClass{classWithCRD(structuredResourceClassWithCRD)},
			objs:    []apiruntime.Object{claimParameters, workerNodeSlice},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `generated class parameters for ResourceClassParameters.example.com default/my-resource-class not found`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},

		"missing-translated-claim-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{claimWithCRD(pendingDelayedClaimWithParams)},
			classes: []*resourcev1alpha2.ResourceClass{classWithCRD(structuredResourceClassWithCRD)},
			objs:    []apiruntime.Object{classParameters, workerNodeSlice},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `generated claim parameters for ResourceClaimParameters.example.com default/my-pod-my-resource not found`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},

		"too-many-translated-class-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{claimWithCRD(pendingDelayedClaimWithParams)},
			classes: []*resourcev1alpha2.ResourceClass{classWithCRD(structuredResourceClassWithCRD)},
			objs:    []apiruntime.Object{claimParameters, classParameters, st.FromClassParameters(classParameters).Name("other").Obj() /* too many */, workerNodeSlice},
			want: want{
				prefilter: result{
					status: framework.AsStatus(errors.New(`multiple generated class parameters for ResourceClassParameters.example.com my-resource-class found: [default/my-resource-class default/other]`)),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},

		"too-many-translated-claim-parameters": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{claimWithCRD(pendingDelayedClaimWithParams)},
			classes: []*resourcev1alpha2.ResourceClass{classWithCRD(structuredResourceClassWithCRD)},
			objs:    []apiruntime.Object{claimParameters, st.FromClaimParameters(claimParameters).Name("other").Obj() /* too many */, classParameters, workerNodeSlice},
			want: want{
				prefilter: result{
					status: framework.AsStatus(errors.New(`multiple generated claim parameters for ResourceClaimParameters.example.com default/my-pod-my-resource found: [default/my-pod-my-resource default/other]`)),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},

		"claim-parameters-CEL-runtime-error": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaimWithParams},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClassWithParams},
			objs:    []apiruntime.Object{breakCELInClaimParameters(claimParameters), classParameters, workerNodeSlice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `checking structured parameters failed: checking node "worker" and resources of driver "some-driver": evaluate request CEL expression: no such key: no-such-attribute`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},

		"class-parameters-CEL-runtime-error": {
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaimWithParams},
			classes: []*resourcev1alpha2.ResourceClass{structuredResourceClassWithParams},
			objs:    []apiruntime.Object{claimParameters, breakCELInClassParameters(classParameters), workerNodeSlice},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `checking structured parameters failed: checking node "worker" and resources of driver "some-driver": evaluate filter CEL expression: no such key: no-such-attribute`),
					},
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},

		"waiting-for-deallocation": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{deallocatingClaim},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim must be reallocated`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"delayed-allocation-missing-class": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("resource class %s does not exist", className)),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"delayed-allocation-scheduling-select-immediately": {
			// Create the PodSchedulingContext object, ask for information
			// and select a node.
			pod:     podWithClaimName,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			classes: []*resourcev1alpha2.ResourceClass{resourceClass},
			want: want{
				prebind: result{
					status: framework.NewStatus(framework.Pending, `waiting for resource driver`),
					added:  []metav1.Object{schedulingSelectedPotential},
				},
			},
		},
		"delayed-allocation-scheduling-ask": {
			// Create the PodSchedulingContext object, ask for
			// information, but do not select a node because
			// there are multiple claims.
			pod:     podWithTwoClaimNames,
			claims:  []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim, pendingDelayedClaim2},
			classes: []*resourcev1alpha2.ResourceClass{resourceClass},
			want: want{
				prebind: result{
					status: framework.NewStatus(framework.Pending, `waiting for resource driver`),
					added:  []metav1.Object{schedulingPotential},
				},
			},
		},
		"delayed-allocation-scheduling-finish": {
			// Use the populated PodSchedulingContext object to select a
			// node.
			pod:         podWithClaimName,
			claims:      []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			schedulings: []*resourcev1alpha2.PodSchedulingContext{schedulingInfo},
			classes:     []*resourcev1alpha2.ResourceClass{resourceClass},
			want: want{
				prebind: result{
					status: framework.NewStatus(framework.Pending, `waiting for resource driver`),
					changes: change{
						scheduling: func(in *resourcev1alpha2.PodSchedulingContext) *resourcev1alpha2.PodSchedulingContext {
							return st.FromPodSchedulingContexts(in).
								SelectedNode(workerNode.Name).
								Obj()
						},
					},
				},
			},
		},
		"delayed-allocation-scheduling-finish-concurrent-label-update": {
			// Use the populated PodSchedulingContext object to select a
			// node.
			pod:         podWithClaimName,
			claims:      []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			schedulings: []*resourcev1alpha2.PodSchedulingContext{schedulingInfo},
			classes:     []*resourcev1alpha2.ResourceClass{resourceClass},
			prepare: prepare{
				prebind: change{
					scheduling: func(in *resourcev1alpha2.PodSchedulingContext) *resourcev1alpha2.PodSchedulingContext {
						// This does not actually conflict with setting the
						// selected node, but because the plugin is not using
						// patching yet, Update nonetheless fails.
						return st.FromPodSchedulingContexts(in).
							Label("hello", "world").
							Obj()
					},
				},
			},
			want: want{
				prebind: result{
					status: framework.AsStatus(errors.New(`ResourceVersion must match the object that gets updated`)),
				},
			},
		},
		"delayed-allocation-scheduling-completed": {
			// Remove PodSchedulingContext object once the pod is scheduled.
			pod:         podWithClaimName,
			claims:      []*resourcev1alpha2.ResourceClaim{allocatedClaim},
			schedulings: []*resourcev1alpha2.PodSchedulingContext{schedulingInfo},
			classes:     []*resourcev1alpha2.ResourceClass{resourceClass},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourcev1alpha2.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
				postbind: result{
					removed: []metav1.Object{schedulingInfo},
				},
			},
		},
		"in-use-by-other": {
			nodes:       []*v1.Node{},
			pod:         otherPodWithClaimName,
			claims:      []*resourcev1alpha2.ResourceClaim{inUseClaim},
			classes:     []*resourcev1alpha2.ResourceClass{},
			schedulings: []*resourcev1alpha2.PodSchedulingContext{},
			prepare:     prepare{},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim in use`),
				},
				postfilter: result{
					status: framework.NewStatus(framework.Unschedulable, `no new claims to deallocate`),
				},
			},
		},
		"wrong-topology-delayed-allocation": {
			// PostFilter tries to get the pod scheduleable by
			// deallocating the claim.
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedDelayedClaimWithWrongTopology},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim not available on the node`),
					},
				},
				postfilter: result{
					// Claims with delayed allocation get deallocated.
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							return st.FromResourceClaim(in).
								DeallocationRequested(true).
								Obj()
						},
					},
					status: framework.NewStatus(framework.Unschedulable, `deallocation of ResourceClaim completed`),
				},
			},
		},
		"wrong-topology-immediate-allocation": {
			// PostFilter tries to get the pod scheduleable by
			// deallocating the claim.
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedImmediateClaimWithWrongTopology},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim not available on the node`),
					},
				},
				postfilter: result{
					// Claims with immediate allocation don't. They would just get allocated again right
					// away, without considering the needs of the pod.
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},
		"wrong-topology-delayed-allocation-structured": {
			// PostFilter tries to get the pod scheduleable by
			// deallocating the claim.
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{structuredAllocatedDelayedClaimWithWrongTopology},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim not available on the node`),
					},
				},
				postfilter: result{
					// Claims with delayed allocation and structured parameters get deallocated immediately.
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							return st.FromResourceClaim(in).
								Allocation("", nil).
								Obj()
						},
					},
					status: framework.NewStatus(framework.Unschedulable, `deallocation of ResourceClaim completed`),
				},
			},
		},
		"wrong-topology-immediate-allocation-structured": {
			// PostFilter tries to get the pod scheduleable by
			// deallocating the claim.
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{structuredAllocatedImmediateClaimWithWrongTopology},
			want: want{
				filter: perNodeResult{
					workerNode.Name: {
						status: framework.NewStatus(framework.UnschedulableAndUnresolvable, `resourceclaim not available on the node`),
					},
				},
				postfilter: result{
					// Claims with immediate allocation don't. The allocation is considered
					// more important than the pod and pods need to wait for the node to
					// become available again.
					status: framework.NewStatus(framework.Unschedulable, `still not schedulable`),
				},
			},
		},
		"good-topology": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedClaimWithGoodTopology},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourcev1alpha2.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
			},
		},
		"bind-failure": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedClaimWithGoodTopology},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourcev1alpha2.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
				unreserveAfterBindFailure: &result{
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							out := in.DeepCopy()
							out.Status.ReservedFor = []resourcev1alpha2.ResourceClaimConsumerReference{}
							return out
						},
					},
				},
			},
		},
		"bind-failure-structured": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{structuredAllocatedClaimWithGoodTopology},
			want: want{
				prebind: result{
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							return st.FromResourceClaim(in).
								ReservedFor(resourcev1alpha2.ResourceClaimConsumerReference{Resource: "pods", Name: podName, UID: types.UID(podUID)}).
								Obj()
						},
					},
				},
				unreserveAfterBindFailure: &result{
					changes: change{
						claim: func(in *resourcev1alpha2.ResourceClaim) *resourcev1alpha2.ResourceClaim {
							out := in.DeepCopy()
							out.Status.ReservedFor = []resourcev1alpha2.ResourceClaimConsumerReference{}
							return out
						},
					},
				},
			},
		},
		"reserved-okay": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{inUseClaim},
		},
		"disable": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{inUseClaim},
			want: want{
				prefilter: result{
					status: framework.NewStatus(framework.Skip),
				},
			},
			disable: true,
		},
	}

	for name, tc := range testcases {
		// We can run in parallel because logging is per-test.
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nodes := tc.nodes
			if nodes == nil {
				nodes = []*v1.Node{workerNode}
			}
			testCtx := setup(t, nodes, tc.claims, tc.classes, tc.schedulings, tc.objs)
			testCtx.p.enabled = !tc.disable
			initialObjects := testCtx.listAll(t)

			status := testCtx.p.PreEnqueue(testCtx.ctx, tc.pod)
			t.Run("PreEnqueue", func(t *testing.T) {
				testCtx.verify(t, tc.want.preenqueue, initialObjects, nil, status)
			})
			if !status.IsSuccess() {
				return
			}

			result, status := testCtx.p.PreFilter(testCtx.ctx, testCtx.state, tc.pod)
			t.Run("prefilter", func(t *testing.T) {
				assert.Equal(t, tc.want.preFilterResult, result)
				testCtx.verify(t, tc.want.prefilter, initialObjects, result, status)
			})
			if status.IsSkip() {
				return
			}
			unschedulable := status.Code() != framework.Success

			var potentialNodes []*framework.NodeInfo

			initialObjects = testCtx.listAll(t)
			testCtx.updateAPIServer(t, initialObjects, tc.prepare.filter)
			if !unschedulable {
				for _, nodeInfo := range testCtx.nodeInfos {
					initialObjects = testCtx.listAll(t)
					status := testCtx.p.Filter(testCtx.ctx, testCtx.state, tc.pod, nodeInfo)
					nodeName := nodeInfo.Node().Name
					t.Run(fmt.Sprintf("filter/%s", nodeInfo.Node().Name), func(t *testing.T) {
						testCtx.verify(t, tc.want.filter.forNode(nodeName), initialObjects, nil, status)
					})
					if status.Code() != framework.Success {
						unschedulable = true
					} else {
						potentialNodes = append(potentialNodes, nodeInfo)
					}
				}
			}

			if !unschedulable && len(potentialNodes) > 0 {
				initialObjects = testCtx.listAll(t)
				initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.prescore)
				status := testCtx.p.PreScore(testCtx.ctx, testCtx.state, tc.pod, potentialNodes)
				t.Run("prescore", func(t *testing.T) {
					testCtx.verify(t, tc.want.prescore, initialObjects, nil, status)
				})
				if status.Code() != framework.Success {
					unschedulable = true
				}
			}

			var selectedNode *framework.NodeInfo
			if !unschedulable && len(potentialNodes) > 0 {
				selectedNode = potentialNodes[0]

				initialObjects = testCtx.listAll(t)
				initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.reserve)
				status := testCtx.p.Reserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
				t.Run("reserve", func(t *testing.T) {
					testCtx.verify(t, tc.want.reserve, initialObjects, nil, status)
				})
				if status.Code() != framework.Success {
					unschedulable = true
				}
			}

			if selectedNode != nil {
				if unschedulable {
					initialObjects = testCtx.listAll(t)
					initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.unreserve)
					testCtx.p.Unreserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
					t.Run("unreserve", func(t *testing.T) {
						testCtx.verify(t, tc.want.unreserve, initialObjects, nil, status)
					})
				} else {
					if tc.want.unreserveBeforePreBind != nil {
						initialObjects = testCtx.listAll(t)
						testCtx.p.Unreserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
						t.Run("unreserveBeforePreBind", func(t *testing.T) {
							testCtx.verify(t, *tc.want.unreserveBeforePreBind, initialObjects, nil, status)
						})
						return
					}

					initialObjects = testCtx.listAll(t)
					initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.prebind)
					status := testCtx.p.PreBind(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
					t.Run("prebind", func(t *testing.T) {
						testCtx.verify(t, tc.want.prebind, initialObjects, nil, status)
					})

					if tc.want.unreserveAfterBindFailure != nil {
						initialObjects = testCtx.listAll(t)
						testCtx.p.Unreserve(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
						t.Run("unreserverAfterBindFailure", func(t *testing.T) {
							testCtx.verify(t, *tc.want.unreserveAfterBindFailure, initialObjects, nil, status)
						})
					} else if status.IsSuccess() {
						initialObjects = testCtx.listAll(t)
						initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.postbind)
						testCtx.p.PostBind(testCtx.ctx, testCtx.state, tc.pod, selectedNode.Node().Name)
						t.Run("postbind", func(t *testing.T) {
							testCtx.verify(t, tc.want.postbind, initialObjects, nil, nil)
						})
					}
				}
			} else {
				initialObjects = testCtx.listAll(t)
				initialObjects = testCtx.updateAPIServer(t, initialObjects, tc.prepare.postfilter)
				result, status := testCtx.p.PostFilter(testCtx.ctx, testCtx.state, tc.pod, nil /* filteredNodeStatusMap not used by plugin */)
				t.Run("postfilter", func(t *testing.T) {
					assert.Equal(t, tc.want.postFilterResult, result)
					testCtx.verify(t, tc.want.postfilter, initialObjects, nil, status)
				})
			}
		})
	}
}

type testContext struct {
	ctx              context.Context
	client           *fake.Clientset
	informerFactory  informers.SharedInformerFactory
	claimAssumeCache *assumecache.AssumeCache
	p                *dynamicResources
	nodeInfos        []*framework.NodeInfo
	state            *framework.CycleState
}

func (tc *testContext) verify(t *testing.T, expected result, initialObjects []metav1.Object, result interface{}, status *framework.Status) {
	t.Helper()
	assert.Equal(t, expected.status, status)
	objects := tc.listAll(t)
	wantObjects := update(t, initialObjects, expected.changes)
	wantObjects = append(wantObjects, expected.added...)
	for _, remove := range expected.removed {
		for i, obj := range wantObjects {
			// This is a bit relaxed (no GVR comparison, no UID
			// comparison) to simplify writing the test cases.
			if obj.GetName() == remove.GetName() && obj.GetNamespace() == remove.GetNamespace() {
				wantObjects = append(wantObjects[0:i], wantObjects[i+1:]...)
				break
			}
		}
	}
	sortObjects(wantObjects)
	// Sometimes assert strips the diff too much, let's do it ourselves...
	if diff := cmp.Diff(wantObjects, objects, cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion")); diff != "" {
		t.Errorf("Stored objects are different (- expected, + actual):\n%s", diff)
	}

	var expectAssumedClaims []metav1.Object
	if expected.assumedClaim != nil {
		expectAssumedClaims = append(expectAssumedClaims, expected.assumedClaim)
	}
	actualAssumedClaims := tc.listAssumedClaims()
	if diff := cmp.Diff(expectAssumedClaims, actualAssumedClaims, cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion")); diff != "" {
		t.Errorf("Assumed claims are different (- expected, + actual):\n%s", diff)
	}

	var expectInFlightClaims []metav1.Object
	if expected.inFlightClaim != nil {
		expectInFlightClaims = append(expectInFlightClaims, expected.inFlightClaim)
	}
	actualInFlightClaims := tc.listInFlightClaims()
	if diff := cmp.Diff(expectInFlightClaims, actualInFlightClaims, cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "ResourceVersion")); diff != "" {
		t.Errorf("In-flight claims are different (- expected, + actual):\n%s", diff)
	}
}

func (tc *testContext) listAll(t *testing.T) (objects []metav1.Object) {
	t.Helper()
	claims, err := tc.client.ResourceV1alpha2().ResourceClaims("").List(tc.ctx, metav1.ListOptions{})
	require.NoError(t, err, "list claims")
	for _, claim := range claims.Items {
		claim := claim
		objects = append(objects, &claim)
	}
	schedulings, err := tc.client.ResourceV1alpha2().PodSchedulingContexts("").List(tc.ctx, metav1.ListOptions{})
	require.NoError(t, err, "list pod scheduling")
	for _, scheduling := range schedulings.Items {
		scheduling := scheduling
		objects = append(objects, &scheduling)
	}

	sortObjects(objects)
	return
}

func (tc *testContext) listAssumedClaims() []metav1.Object {
	var assumedClaims []metav1.Object
	for _, obj := range tc.p.claimAssumeCache.List(nil) {
		claim := obj.(*resourcev1alpha2.ResourceClaim)
		obj, _ := tc.p.claimAssumeCache.Get(claim.Namespace + "/" + claim.Name)
		apiObj, _ := tc.p.claimAssumeCache.GetAPIObj(claim.Namespace + "/" + claim.Name)
		if obj != apiObj {
			assumedClaims = append(assumedClaims, claim)
		}
	}
	sortObjects(assumedClaims)
	return assumedClaims
}

func (tc *testContext) listInFlightClaims() []metav1.Object {
	var inFlightClaims []metav1.Object
	tc.p.inFlightAllocations.Range(func(key, value any) bool {
		inFlightClaims = append(inFlightClaims, value.(*resourcev1alpha2.ResourceClaim))
		return true
	})
	sortObjects(inFlightClaims)
	return inFlightClaims
}

// updateAPIServer modifies objects and stores any changed object in the API server.
func (tc *testContext) updateAPIServer(t *testing.T, objects []metav1.Object, updates change) []metav1.Object {
	modified := update(t, objects, updates)
	for i := range modified {
		obj := modified[i]
		if diff := cmp.Diff(objects[i], obj); diff != "" {
			t.Logf("Updating %T %q, diff (-old, +new):\n%s", obj, obj.GetName(), diff)
			switch obj := obj.(type) {
			case *resourcev1alpha2.ResourceClaim:
				obj, err := tc.client.ResourceV1alpha2().ResourceClaims(obj.Namespace).Update(tc.ctx, obj, metav1.UpdateOptions{})
				if err != nil {
					t.Fatalf("unexpected error during prepare update: %v", err)
				}
				modified[i] = obj
			case *resourcev1alpha2.PodSchedulingContext:
				obj, err := tc.client.ResourceV1alpha2().PodSchedulingContexts(obj.Namespace).Update(tc.ctx, obj, metav1.UpdateOptions{})
				if err != nil {
					t.Fatalf("unexpected error during prepare update: %v", err)
				}
				modified[i] = obj
			default:
				t.Fatalf("unsupported object type %T", obj)
			}
		}
	}
	return modified
}

func sortObjects(objects []metav1.Object) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].GetNamespace() < objects[j].GetNamespace() {
			return true
		}
		return objects[i].GetName() < objects[j].GetName()
	})
}

// update walks through all existing objects, finds the corresponding update
// function based on name and kind, and replaces those objects that have an
// update function. The rest is left unchanged.
func update(t *testing.T, objects []metav1.Object, updates change) []metav1.Object {
	var updated []metav1.Object

	for _, obj := range objects {
		switch in := obj.(type) {
		case *resourcev1alpha2.ResourceClaim:
			if updates.claim != nil {
				obj = updates.claim(in)
			}
		case *resourcev1alpha2.PodSchedulingContext:
			if updates.scheduling != nil {
				obj = updates.scheduling(in)
			}
		}
		updated = append(updated, obj)
	}

	return updated
}

func setup(t *testing.T, nodes []*v1.Node, claims []*resourcev1alpha2.ResourceClaim, classes []*resourcev1alpha2.ResourceClass, schedulings []*resourcev1alpha2.PodSchedulingContext, objs []apiruntime.Object) (result *testContext) {
	t.Helper()

	tc := &testContext{}
	tCtx := ktesting.Init(t)
	tc.ctx = tCtx

	tc.client = fake.NewSimpleClientset(objs...)
	reactor := createReactor(tc.client.Tracker())
	tc.client.PrependReactor("*", "*", reactor)

	// Quick-and-dirty workaround for fake client storing ResourceClassParameters and
	// ResourceClaimParameters as "resourceclassparameterses" and "resourceclaimparameterses":
	// intercept the correct LIST from the informers and reply to them with the incorrect
	// LIST result.
	tc.client.PrependReactor("list", "resourceclaimparameters", createListReactor(tc.client.Tracker(), "ResourceClaimParameters"))
	tc.client.PrependReactor("list", "resourceclassparameters", createListReactor(tc.client.Tracker(), "ResourceClassParameters"))

	tc.informerFactory = informers.NewSharedInformerFactory(tc.client, 0)
	tc.claimAssumeCache = assumecache.NewAssumeCache(tCtx.Logger(), tc.informerFactory.Resource().V1alpha2().ResourceClaims().Informer(), "resource claim", "", nil)
	opts := []runtime.Option{
		runtime.WithClientSet(tc.client),
		runtime.WithInformerFactory(tc.informerFactory),
		runtime.WithResourceClaimCache(tc.claimAssumeCache),
	}
	fh, err := runtime.NewFramework(tCtx, nil, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}

	pl, err := New(tCtx, nil, fh, feature.Features{EnableDynamicResourceAllocation: true})
	if err != nil {
		t.Fatal(err)
	}
	tc.p = pl.(*dynamicResources)

	// The tests use the API to create the objects because then reactors
	// get triggered.
	for _, claim := range claims {
		_, err := tc.client.ResourceV1alpha2().ResourceClaims(claim.Namespace).Create(tc.ctx, claim, metav1.CreateOptions{})
		require.NoError(t, err, "create resource claim")
	}
	for _, class := range classes {
		_, err := tc.client.ResourceV1alpha2().ResourceClasses().Create(tc.ctx, class, metav1.CreateOptions{})
		require.NoError(t, err, "create resource class")
	}
	for _, scheduling := range schedulings {
		_, err := tc.client.ResourceV1alpha2().PodSchedulingContexts(scheduling.Namespace).Create(tc.ctx, scheduling, metav1.CreateOptions{})
		require.NoError(t, err, "create pod scheduling")
	}

	tc.informerFactory.Start(tc.ctx.Done())
	t.Cleanup(func() {
		// Need to cancel before waiting for the shutdown.
		tCtx.Cancel("test is done")
		// Now we can wait for all goroutines to stop.
		tc.informerFactory.Shutdown()
	})

	tc.informerFactory.WaitForCacheSync(tc.ctx.Done())

	for _, node := range nodes {
		nodeInfo := framework.NewNodeInfo()
		nodeInfo.SetNode(node)
		tc.nodeInfos = append(tc.nodeInfos, nodeInfo)
	}
	tc.state = framework.NewCycleState()

	return tc
}

// createReactor implements the logic required for the UID and ResourceVersion
// fields to work when using the fake client. Add it with client.PrependReactor
// to your fake client. ResourceVersion handling is required for conflict
// detection during updates, which is covered by some scenarios.
func createReactor(tracker cgotesting.ObjectTracker) func(action cgotesting.Action) (handled bool, ret apiruntime.Object, err error) {
	var uidCounter int
	var resourceVersionCounter int
	var mutex sync.Mutex

	return func(action cgotesting.Action) (handled bool, ret apiruntime.Object, err error) {
		createAction, ok := action.(cgotesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := createAction.GetObject().(metav1.Object)
		if !ok {
			return false, nil, nil
		}

		mutex.Lock()
		defer mutex.Unlock()
		switch action.GetVerb() {
		case "create":
			if obj.GetUID() != "" {
				return true, nil, errors.New("UID must not be set on create")
			}
			if obj.GetResourceVersion() != "" {
				return true, nil, errors.New("ResourceVersion must not be set on create")
			}
			obj.SetUID(types.UID(fmt.Sprintf("UID-%d", uidCounter)))
			uidCounter++
			obj.SetResourceVersion(fmt.Sprintf("%d", resourceVersionCounter))
			resourceVersionCounter++
		case "update":
			uid := obj.GetUID()
			resourceVersion := obj.GetResourceVersion()
			if uid == "" {
				return true, nil, errors.New("UID must be set on update")
			}
			if resourceVersion == "" {
				return true, nil, errors.New("ResourceVersion must be set on update")
			}

			oldObj, err := tracker.Get(action.GetResource(), obj.GetNamespace(), obj.GetName())
			if err != nil {
				return true, nil, err
			}
			oldObjMeta, ok := oldObj.(metav1.Object)
			if !ok {
				return true, nil, errors.New("internal error: unexpected old object type")
			}
			if oldObjMeta.GetResourceVersion() != resourceVersion {
				return true, nil, errors.New("ResourceVersion must match the object that gets updated")
			}

			obj.SetResourceVersion(fmt.Sprintf("%d", resourceVersionCounter))
			resourceVersionCounter++
		}
		return false, nil, nil
	}
}

func createListReactor(tracker cgotesting.ObjectTracker, kind string) func(action cgotesting.Action) (handled bool, ret apiruntime.Object, err error) {
	return func(action cgotesting.Action) (handled bool, ret apiruntime.Object, err error) {
		// listAction := action.(cgotesting.ListAction)
		gvr := action.GetResource()
		ns := action.GetNamespace()
		gvr.Resource += "es"
		list, err := tracker.List(gvr, schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: kind}, ns)
		return true, list, err
	}
}

func Test_isSchedulableAfterClaimChange(t *testing.T) {
	testcases := map[string]struct {
		pod            *v1.Pod
		claims         []*resourcev1alpha2.ResourceClaim
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		"skip-deletes": {
			pod:          podWithClaimTemplate,
			oldObj:       allocatedClaim,
			newObj:       nil,
			expectedHint: framework.QueueSkip,
		},
		"backoff-wrong-new-object": {
			pod:         podWithClaimTemplate,
			newObj:      "not-a-claim",
			expectedErr: true,
		},
		"skip-wrong-claim": {
			pod: podWithClaimTemplate,
			newObj: func() *resourcev1alpha2.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.OwnerReferences[0].UID += "123"
				return claim
			}(),
			expectedHint: framework.QueueSkip,
		},
		"skip-unrelated-claim": {
			pod:    podWithClaimTemplate,
			claims: []*resourcev1alpha2.ResourceClaim{allocatedClaim},
			newObj: func() *resourcev1alpha2.ResourceClaim {
				claim := allocatedClaim.DeepCopy()
				claim.Name += "-foo"
				claim.UID += "123"
				return claim
			}(),
			expectedHint: framework.QueueSkip,
		},
		"queue-on-add": {
			pod:          podWithClaimName,
			newObj:       pendingImmediateClaim,
			expectedHint: framework.Queue,
		},
		"backoff-wrong-old-object": {
			pod:         podWithClaimName,
			claims:      []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			oldObj:      "not-a-claim",
			newObj:      pendingImmediateClaim,
			expectedErr: true,
		},
		"skip-adding-finalizer": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{pendingImmediateClaim},
			oldObj: pendingImmediateClaim,
			newObj: func() *resourcev1alpha2.ResourceClaim {
				claim := pendingImmediateClaim.DeepCopy()
				claim.Finalizers = append(claim.Finalizers, "foo")
				return claim
			}(),
			expectedHint: framework.QueueSkip,
		},
		"queue-on-status-change": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{pendingImmediateClaim},
			oldObj: pendingImmediateClaim,
			newObj: func() *resourcev1alpha2.ResourceClaim {
				claim := pendingImmediateClaim.DeepCopy()
				claim.Status.Allocation = &resourcev1alpha2.AllocationResult{}
				return claim
			}(),
			expectedHint: framework.Queue,
		},
		"structured-claim-deallocate": {
			pod:    podWithClaimName,
			claims: []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim, otherStructuredAllocatedClaim},
			oldObj: otherStructuredAllocatedClaim,
			newObj: func() *resourcev1alpha2.ResourceClaim {
				claim := otherStructuredAllocatedClaim.DeepCopy()
				claim.Status.Allocation = nil
				return claim
			}(),
			// TODO (https://github.com/kubernetes/kubernetes/issues/123697): don't wake up
			// claims not using structured parameters.
			expectedHint: framework.Queue,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			logger, tCtx := ktesting.NewTestContext(t)
			testCtx := setup(t, nil, tc.claims, nil, nil, nil)
			oldObj := tc.oldObj
			newObj := tc.newObj
			if claim, ok := tc.newObj.(*resourcev1alpha2.ResourceClaim); ok {
				// Add or update through the client and wait until the event is processed.
				claimKey := claim.Namespace + "/" + claim.Name
				if tc.oldObj == nil {
					// Some test claims already have it. Clear for create.
					createClaim := claim.DeepCopy()
					createClaim.UID = ""
					storedClaim, err := testCtx.client.ResourceV1alpha2().ResourceClaims(createClaim.Namespace).Create(tCtx, createClaim, metav1.CreateOptions{})
					require.NoError(t, err, "create claim")
					claim = storedClaim
				} else {
					cachedClaim, err := testCtx.claimAssumeCache.Get(claimKey)
					require.NoError(t, err, "retrieve old claim")
					updateClaim := claim.DeepCopy()
					// The test claim doesn't have those (generated dynamically), so copy them.
					updateClaim.UID = cachedClaim.(*resourcev1alpha2.ResourceClaim).UID
					updateClaim.ResourceVersion = cachedClaim.(*resourcev1alpha2.ResourceClaim).ResourceVersion

					storedClaim, err := testCtx.client.ResourceV1alpha2().ResourceClaims(updateClaim.Namespace).Update(tCtx, updateClaim, metav1.UpdateOptions{})
					require.NoError(t, err, "update claim")
					claim = storedClaim
				}

				// Eventually the assume cache will have it, too.
				require.EventuallyWithT(t, func(t *assert.CollectT) {
					cachedClaim, err := testCtx.claimAssumeCache.Get(claimKey)
					require.NoError(t, err, "retrieve claim")
					if cachedClaim.(*resourcev1alpha2.ResourceClaim).ResourceVersion != claim.ResourceVersion {
						t.Errorf("cached claim not updated yet")
					}
				}, time.Minute, time.Second, "claim assume cache must have new or updated claim")

				// This has the actual UID and ResourceVersion,
				// which is relevant for
				// isSchedulableAfterClaimChange.
				newObj = claim
			}
			actualHint, err := testCtx.p.isSchedulableAfterClaimChange(logger, tc.pod, oldObj, newObj)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedHint, actualHint)
		})
	}
}

func Test_isSchedulableAfterPodSchedulingContextChange(t *testing.T) {
	testcases := map[string]struct {
		pod            *v1.Pod
		schedulings    []*resourcev1alpha2.PodSchedulingContext
		claims         []*resourcev1alpha2.ResourceClaim
		oldObj, newObj interface{}
		expectedHint   framework.QueueingHint
		expectedErr    bool
	}{
		"skip-deleted": {
			pod:          podWithClaimTemplate,
			oldObj:       scheduling,
			expectedHint: framework.QueueSkip,
		},
		"skip-missed-deleted": {
			pod: podWithClaimTemplate,
			oldObj: cache.DeletedFinalStateUnknown{
				Obj: scheduling,
			},
			expectedHint: framework.QueueSkip,
		},
		"backoff-wrong-old-object": {
			pod:         podWithClaimTemplate,
			oldObj:      "not-a-scheduling-context",
			newObj:      scheduling,
			expectedErr: true,
		},
		"backoff-missed-wrong-old-object": {
			pod: podWithClaimTemplate,
			oldObj: cache.DeletedFinalStateUnknown{
				Obj: "not-a-scheduling-context",
			},
			newObj:      scheduling,
			expectedErr: true,
		},
		"skip-unrelated-object": {
			pod:    podWithClaimTemplate,
			claims: []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			newObj: func() *resourcev1alpha2.PodSchedulingContext {
				scheduling := scheduling.DeepCopy()
				scheduling.Name += "-foo"
				return scheduling
			}(),
			expectedHint: framework.QueueSkip,
		},
		"backoff-wrong-new-object": {
			pod:         podWithClaimTemplate,
			oldObj:      scheduling,
			newObj:      "not-a-scheduling-context",
			expectedErr: true,
		},
		"skip-missing-claim": {
			pod:          podWithClaimTemplate,
			oldObj:       scheduling,
			newObj:       schedulingInfo,
			expectedHint: framework.QueueSkip,
		},
		"skip-missing-infos": {
			pod:          podWithClaimTemplateInStatus,
			claims:       []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			oldObj:       scheduling,
			newObj:       scheduling,
			expectedHint: framework.QueueSkip,
		},
		"queue-new-infos": {
			pod:          podWithClaimTemplateInStatus,
			claims:       []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			oldObj:       scheduling,
			newObj:       schedulingInfo,
			expectedHint: framework.Queue,
		},
		"queue-bad-selected-node": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			oldObj: func() *resourcev1alpha2.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Spec.SelectedNode = workerNode.Name
				return scheduling
			}(),
			newObj: func() *resourcev1alpha2.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Spec.SelectedNode = workerNode.Name
				scheduling.Status.ResourceClaims[0].UnsuitableNodes = append(scheduling.Status.ResourceClaims[0].UnsuitableNodes, scheduling.Spec.SelectedNode)
				return scheduling
			}(),
			expectedHint: framework.Queue,
		},
		"skip-spec-changes": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			oldObj: schedulingInfo,
			newObj: func() *resourcev1alpha2.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Spec.SelectedNode = workerNode.Name
				return scheduling
			}(),
			expectedHint: framework.QueueSkip,
		},
		"backoff-other-changes": {
			pod:    podWithClaimTemplateInStatus,
			claims: []*resourcev1alpha2.ResourceClaim{pendingDelayedClaim},
			oldObj: schedulingInfo,
			newObj: func() *resourcev1alpha2.PodSchedulingContext {
				scheduling := schedulingInfo.DeepCopy()
				scheduling.Finalizers = append(scheduling.Finalizers, "foo")
				return scheduling
			}(),
			expectedHint: framework.Queue,
		},
	}

	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logger, _ := ktesting.NewTestContext(t)
			testCtx := setup(t, nil, tc.claims, nil, tc.schedulings, nil)
			actualHint, err := testCtx.p.isSchedulableAfterPodSchedulingContextChange(logger, tc.pod, tc.oldObj, tc.newObj)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedHint, actualHint)
		})
	}
}
