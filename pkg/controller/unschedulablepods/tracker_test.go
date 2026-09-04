/*
Copyright The Kubernetes Authors.

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

package unschedulablepods

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/featuregate"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/constants"
	controllerconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/controller/core/indexer"
	podconstants "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/util/api"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingpod "sigs.k8s.io/kueue/pkg/util/testingjobs/pod"
	"sigs.k8s.io/kueue/pkg/workloadslicing"
)

const (
	testNamespace   = "ns"
	testWorkload    = "wl"
	testWorkloadUID = types.UID("wl-uid")
	testPodSet      = kueue.PodSetReference("main")
)

var (
	errList              = errors.New("list failed")
	errGetAdmissionCheck = errors.New("get admission check failed")
)

func TestSummarizeScheduling(t *testing.T) {
	now := time.Now()
	pod := func(name string, podSet kueue.PodSetReference) *testingpod.PodWrapper {
		return testingpod.MakePod(name, testNamespace).Label(constants.PodSetLabel, string(podSet))
	}
	scheduledCondition := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}
	grant := func(count int32) map[kueue.PodSetReference]int32 {
		return map[kueue.PodSetReference]int32{testPodSet: count}
	}

	testCases := map[string]struct {
		granted                map[kueue.PodSetReference]int32
		pods                   []*corev1.Pod
		reclaimable            []kueue.ReclaimablePod
		disableReclaimablePods bool
		want                   schedulingSummary
	}{
		"no pods": {
			granted: grant(2),
			want:    schedulingSummary{required: 2},
		},
		"pending pod without conditions": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).Obj()},
			want:    schedulingSummary{required: 1, nonTerminal: 1},
		},
		"scheduling-gated pod": {
			granted: grant(1),
			pods: []*corev1.Pod{
				pod("p1", testPodSet).Gate("example.com/gate").
					StatusConditions(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonSchedulingGated}).
					Obj(),
			},
			want: schedulingSummary{required: 1, nonTerminal: 1},
		},
		"pod bound through the PodScheduled condition": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).StatusConditions(scheduledCondition).Obj()},
			want:    schedulingSummary{scheduled: 1, required: 1, nonTerminal: 1},
		},
		"pod bound through a preset nodeName": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").Obj()},
			want:    schedulingSummary{scheduled: 1, required: 1, nonTerminal: 1},
		},
		"terminating scheduled pod is neither scheduled nor live": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").DeletionTimestamp(now).Obj()},
			want:    schedulingSummary{required: 1},
		},
		"terminating failed pod does not count": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").DeletionTimestamp(now).StatusPhase(corev1.PodFailed).Obj()},
			want:    schedulingSummary{required: 1},
		},
		"terminating succeeded pod still counts": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").DeletionTimestamp(now).StatusPhase(corev1.PodSucceeded).Obj()},
			want:    schedulingSummary{scheduled: 1, required: 1, succeededFillsGrant: true},
		},
		"succeeded pod counts even without a node": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).StatusPhase(corev1.PodSucceeded).Obj()},
			want:    schedulingSummary{scheduled: 1, required: 1, succeededFillsGrant: true},
		},
		"succeeded pods below the grant do not fill it": {
			granted: grant(2),
			pods:    []*corev1.Pod{pod("p1", testPodSet).StatusPhase(corev1.PodSucceeded).Obj()},
			want:    schedulingSummary{scheduled: 1, required: 2},
		},
		"succeeded pods are capped per podset when filling the grant": {
			granted: map[kueue.PodSetReference]int32{"leader": 1, "worker": 1},
			pods: []*corev1.Pod{
				pod("l1", "leader").StatusPhase(corev1.PodSucceeded).Obj(),
				pod("l2", "leader").StatusPhase(corev1.PodSucceeded).Obj(),
			},
			want: schedulingSummary{scheduled: 1, required: 2},
		},
		"failed pod does not count": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").StatusPhase(corev1.PodFailed).Obj()},
			want:    schedulingSummary{required: 1},
		},
		"granted zero with a failed pod only": {
			granted: grant(0),
			pods:    []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").StatusPhase(corev1.PodFailed).Obj()},
			want:    schedulingSummary{},
		},
		"reclaimable pods count": {
			granted:     grant(2),
			pods:        []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").Obj()},
			reclaimable: []kueue.ReclaimablePod{{Name: testPodSet, Count: 1}},
			want:        schedulingSummary{scheduled: 2, required: 2, nonTerminal: 1},
		},
		"reclaimable pods do not fill the grant on their own": {
			granted:     grant(2),
			reclaimable: []kueue.ReclaimablePod{{Name: testPodSet, Count: 2}},
			want:        schedulingSummary{scheduled: 2, required: 2},
		},
		"reclaimable pods are ignored when the feature is disabled": {
			granted:                grant(2),
			pods:                   []*corev1.Pod{pod("p1", testPodSet).NodeName("node-a").Obj()},
			reclaimable:            []kueue.ReclaimablePod{{Name: testPodSet, Count: 1}},
			disableReclaimablePods: true,
			want:                   schedulingSummary{scheduled: 1, required: 2, nonTerminal: 1},
		},
		"succeeded and reclaimable pods are not summed": {
			granted: grant(3),
			pods: []*corev1.Pod{
				pod("p1", testPodSet).NodeName("node-a").Obj(),
				pod("p2", testPodSet).StatusPhase(corev1.PodSucceeded).Obj(),
			},
			reclaimable: []kueue.ReclaimablePod{{Name: testPodSet, Count: 1}},
			want:        schedulingSummary{scheduled: 2, required: 3, nonTerminal: 1},
		},
		"surplus pods are capped at the granted count": {
			granted: grant(1),
			pods: []*corev1.Pod{
				pod("p1", testPodSet).NodeName("node-a").Obj(),
				pod("p2", testPodSet).NodeName("node-a").Obj(),
			},
			want: schedulingSummary{scheduled: 1, required: 1, nonTerminal: 2},
		},
		"pod of an unknown podset does not count": {
			granted: grant(1),
			pods:    []*corev1.Pod{pod("p1", "other").NodeName("node-a").Obj()},
			want:    schedulingSummary{required: 1, nonTerminal: 1},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.ReclaimablePods, !tc.disableReclaimablePods)
			podSets := make([]kueue.PodSet, 0, len(tc.granted))
			assignments := make([]kueue.PodSetAssignment, 0, len(tc.granted))
			for name, count := range tc.granted {
				podSets = append(podSets, *utiltestingapi.MakePodSet(name, int(count)).Request(corev1.ResourceCPU, "1").Obj())
				assignments = append(assignments, utiltestingapi.MakePodSetAssignment(name).Count(count).Obj())
			}
			wl := utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				PodSets(podSets...).
				ReserveQuotaAt(utiltestingapi.MakeAdmission("cq").PodSets(assignments...).Obj(), now).
				ReclaimablePods(tc.reclaimable...).
				Obj()
			pods := make([]corev1.Pod, 0, len(tc.pods))
			for _, p := range tc.pods {
				pods = append(pods, *p)
			}
			if got := summarizeScheduling(wl, pods); got != tc.want {
				t.Errorf("summarizeScheduling() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLifecycleTarget(t *testing.T) {
	now := time.Now()
	admission := utiltestingapi.MakeAdmission("cq").Obj()
	condition := func(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
		return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message, LastTransitionTime: metav1.NewTime(now)}
	}
	longMessage := strings.Repeat("x", 40*1024)

	testCases := map[string]struct {
		features    map[featuregate.Feature]bool
		workload    *kueue.Workload
		wantReason  string
		wantMessage string
		wantOK      bool
	}{
		"evicted workload holding its quota takes the eviction reason": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				ReserveQuotaAt(admission, now).
				Condition(condition(kueue.WorkloadEvicted, metav1.ConditionTrue, kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				Obj(),
			wantReason:  kueue.WorkloadEvictedByPodsReadyTimeout,
			wantMessage: "Exceeded the PodsReady timeout",
			wantOK:      true,
		},
		"evicted workload takes the eviction reason over the quota release": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Condition(condition(kueue.WorkloadEvicted, metav1.ConditionTrue, kueue.WorkloadEvictedByPreemption, "Preempted")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, unscheduledPodsMessage)).
				Obj(),
			wantReason:  kueue.WorkloadEvictedByPreemption,
			wantMessage: "Preempted",
			wantOK:      true,
		},
		"released workload takes the quota release reason": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, unscheduledPodsMessage)).
				Obj(),
			wantReason:  kueue.WorkloadOnHold,
			wantMessage: "On hold",
			wantOK:      true,
		},
		"empty message falls back to the quota release message": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, "Pending", "")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				Obj(),
			wantReason:  "Pending",
			wantMessage: quotaReleasedMessage,
			wantOK:      true,
		},
		"long message is truncated": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, "Pending", longMessage)).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				Obj(),
			wantReason:  "Pending",
			wantMessage: api.TruncateConditionMessage(longMessage),
			wantOK:      true,
		},
		"reason change of an already reset condition": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				ReserveQuotaAt(admission, now).
				Condition(condition(kueue.WorkloadEvicted, metav1.ConditionTrue, kueue.WorkloadDeactivated, "Deactivated")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionFalse, kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
				Obj(),
			wantReason:  kueue.WorkloadDeactivated,
			wantMessage: "Deactivated",
			wantOK:      true,
		},
		"condition already carrying the target": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Obj(),
		},
		"finished workload keeps its observation": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				FinishedAt(now).
				Obj(),
		},
		"variant workload is skipped": {
			features: map[featuregate.Feature]bool{features.ConcurrentAdmission: true},
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				OwnerReference(kueue.SchemeGroupVersion.WithKind("Workload"), "parent", "parent-uid").
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				Obj(),
		},
		"workload without the condition": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, kueue.WorkloadOnHold, "On hold")).
				Obj(),
		},
		"workload holding its quota without being evicted": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				ReserveQuotaAt(admission, now).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				Obj(),
		},
		"workload without any quota reservation condition": {
			workload: utiltestingapi.MakeWorkload(testWorkload, testNamespace).
				Condition(condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage)).
				Obj(),
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			for feature, enabled := range tc.features {
				features.SetFeatureGateDuringTest(t, feature, enabled)
			}
			gotReason, gotMessage, gotOK := lifecycleTarget(tc.workload)
			if gotReason != tc.wantReason || gotMessage != tc.wantMessage || gotOK != tc.wantOK {
				t.Errorf("lifecycleTarget() = (%q, %q, %t), want (%q, %q, %t)", gotReason, gotMessage, gotOK, tc.wantReason, tc.wantMessage, tc.wantOK)
			}
		})
	}
}

func TestReconcile(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	earlier := now.Add(-time.Minute)
	muchEarlier := now.Add(-time.Hour)

	podSet := func(name kueue.PodSetReference, count int) kueue.PodSet {
		return *utiltestingapi.MakePodSet(name, count).Request(corev1.ResourceCPU, "1").Obj()
	}
	admission := func(podSets ...kueue.PodSet) *kueue.Admission {
		assignments := make([]kueue.PodSetAssignment, 0, len(podSets))
		for _, ps := range podSets {
			assignments = append(assignments, utiltestingapi.MakePodSetAssignment(ps.Name).Count(ps.Count).Obj())
		}
		return utiltestingapi.MakeAdmission("cq").PodSets(assignments...).Obj()
	}
	admittedWorkloadAt := func(name string, admittedAt time.Time, podSets ...kueue.PodSet) *utiltestingapi.WorkloadWrapper {
		return utiltestingapi.MakeWorkload(name, testNamespace).
			UID(testWorkloadUID).
			PodSets(podSets...).
			ReserveQuotaAt(admission(podSets...), admittedAt).
			AdmittedAt(true, admittedAt)
	}
	admittedWorkload := func(podSets ...kueue.PodSet) *utiltestingapi.WorkloadWrapper {
		return admittedWorkloadAt(testWorkload, earlier, podSets...)
	}
	condition := func(condType string, status metav1.ConditionStatus, reason, message string, transition time.Time) metav1.Condition {
		return metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.NewTime(transition),
		}
	}
	evicted := func(reason, message string) metav1.Condition {
		return condition(kueue.WorkloadEvicted, metav1.ConditionTrue, reason, message, now)
	}
	quotaReleased := func(reason, message string) metav1.Condition {
		return condition(kueue.WorkloadQuotaReserved, metav1.ConditionFalse, reason, message, now)
	}
	releasedWorkload := func(reason, message string, podSets ...kueue.PodSet) *utiltestingapi.WorkloadWrapper {
		return utiltestingapi.MakeWorkload(testWorkload, testNamespace).
			UID(testWorkloadUID).
			PodSets(podSets...).
			Condition(quotaReleased(reason, message))
	}
	pod := func(name string, podSet kueue.PodSetReference) *testingpod.PodWrapper {
		return testingpod.MakePod(name, testNamespace).
			Annotation(kueue.WorkloadAnnotation, testWorkload).
			Label(constants.PodSetLabel, string(podSet))
	}
	scheduled := func(p *testingpod.PodWrapper) *testingpod.PodWrapper {
		return p.StatusConditions(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue})
	}
	terminating := func(p *testingpod.PodWrapper) *testingpod.PodWrapper {
		return p.DeletionTimestamp(now).Finalizer("example.com/finalizer")
	}
	waitForScheduling := func(transition time.Time) metav1.Condition {
		return condition(kueue.WorkloadPodsScheduled, metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, unscheduledPodsMessage, transition)
	}
	allScheduled := func(transition time.Time) metav1.Condition {
		return condition(kueue.WorkloadPodsScheduled, metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, allPodsScheduledMessage, transition)
	}
	lifecycle := func(reason, message string, transition time.Time) metav1.Condition {
		return condition(kueue.WorkloadPodsScheduled, metav1.ConditionFalse, reason, message, transition)
	}
	multiKueueCheck := utiltestingapi.MakeAdmissionCheck("mk").ControllerName(kueue.MultiKueueControllerName).Obj()
	multiKueueState := kueue.AdmissionCheckState{Name: "mk", State: kueue.CheckStateReady}
	readmit := func(wl *kueue.Workload) {
		apimeta.RemoveStatusCondition(&wl.Status.Conditions, kueue.WorkloadEvicted)
		apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadAdmitted).LastTransitionTime = metav1.NewTime(now)
	}

	testCases := map[string]struct {
		features             map[featuregate.Feature]bool
		request              *types.NamespacedName
		workloads            []*kueue.Workload
		admissionChecks      []*kueue.AdmissionCheck
		pods                 []*corev1.Pod
		listPodsErr          error
		getAdmissionCheckErr error
		updateBeforePatch    func(*kueue.Workload)
		wantResult           reconcile.Result
		wantErr              error
		wantConflict         bool
		reconcileAgain       bool
		wantSecondResult     reconcile.Result
		wantConditions       map[string]*metav1.Condition
	}{
		"evicted workload holding its quota is reset to the eviction reason": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout", now)),
			},
		},
		"evicted workload already reset is left untouched": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					Condition(lifecycle(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout", now.Add(-30*time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout", now.Add(-30*time.Second))),
			},
		},
		"eviction reason rewritten by deactivation is reset again keeping the transition time": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(evicted(kueue.WorkloadDeactivated, "The workload is deactivated")).
					Condition(lifecycle(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout", now.Add(-30*time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadDeactivated, "The workload is deactivated", now.Add(-30*time.Second))),
			},
		},
		"released workload is reset to the quota release reason": {
			workloads: []*kueue.Workload{
				releasedWorkload(kueue.WorkloadOnHold, "The workload is on hold", podSet(testPodSet, 1)).
					Condition(allScheduled(earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", now)),
			},
		},
		"released workload is reset to the quota release reason with the merge patch": {
			features: map[featuregate.Feature]bool{features.WorkloadRequestUseMergePatch: true},
			workloads: []*kueue.Workload{
				releasedWorkload(kueue.WorkloadOnHold, "The workload is on hold", podSet(testPodSet, 1)).
					Condition(allScheduled(earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", now)),
			},
		},
		"consecutive release with the same reason and another message is written keeping the transition time": {
			workloads: []*kueue.Workload{
				releasedWorkload(kueue.WorkloadOnHold, "The workload is on hold again", podSet(testPodSet, 1)).
					Condition(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadOnHold, "The workload is on hold again", earlier)),
			},
		},
		"consecutive release with the same values is not written": {
			workloads: []*kueue.Workload{
				releasedWorkload(kueue.WorkloadOnHold, "The workload is on hold", podSet(testPodSet, 1)).
					Condition(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", earlier)),
			},
		},
		"release with another reason after an admission without pods is written keeping the transition time": {
			workloads: []*kueue.Workload{
				releasedWorkload("Pending", "The workload is pending", podSet(testPodSet, 1)).
					Condition(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle("Pending", "The workload is pending", earlier)),
			},
		},
		"release without a message falls back to the quota release message": {
			workloads: []*kueue.Workload{
				releasedWorkload("Pending", "", podSet(testPodSet, 1)).
					Condition(allScheduled(earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(lifecycle("Pending", quotaReleasedMessage, now)),
			},
		},
		"evicted finished workload keeps its condition": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					FinishedAt(now).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"evicted workload without the condition is left untouched": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"concurrent admission: an evicted variant workload keeps its condition": {
			features: map[featuregate.Feature]bool{features.ConcurrentAdmission: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					OwnerReference(kueue.SchemeGroupVersion.WithKind("Workload"), "parent", "parent-uid").
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"re-admitted workload with an observation of the previous admission and no pods is left untouched": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(allScheduled(muchEarlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(muchEarlier)),
			},
		},
		"re-admitted workload with an observation of the previous admission restamps it from the pods": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(allScheduled(muchEarlier)).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"reset conflicting with a re-admission is retried without resetting": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			updateBeforePatch: readmit,
			wantConflict:      true,
			reconcileAgain:    true,
			wantSecondResult:  reconcile.Result{RequeueAfter: time.Second},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"re-admission between the pod list and the patch makes the patch conflict": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			updateBeforePatch: readmit,
			wantConflict:      true,
			wantConditions:    map[string]*metav1.Condition{testWorkload: nil},
		},
		"first observation after a reset is stamped now": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(lifecycle(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout", now.Add(-30*time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"condition stamped in the admission second is restamped with the same status": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(waitForScheduling(earlier)).
					Obj(),
			},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"condition stamped in the admission second is restamped with the same status with the merge patch": {
			features: map[featuregate.Feature]bool{features.WorkloadRequestUseMergePatch: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(waitForScheduling(earlier)).
					Obj(),
			},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"condition stamped in the admission second is replaced by the opposite observation (True to False)": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(allScheduled(earlier)).
					Obj(),
			},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"condition stamped in the admission second is replaced by the opposite observation (False to True)": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Condition(waitForScheduling(earlier)).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"condition of a previous admission is restamped even with the same status": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(allScheduled(muchEarlier)).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"concurrent admission: a parent workload is observed": {
			features: map[featuregate.Feature]bool{features.ConcurrentAdmission: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Label(controllerconstants.ConcurrentAdmissionParentLabelKey, "true").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"concurrent admission: a variant workload is not observed": {
			features: map[featuregate.Feature]bool{features.ConcurrentAdmission: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					OwnerReference(kueue.SchemeGroupVersion.WithKind("Workload"), "parent", "parent-uid").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"workload without quota reservation is skipped": {
			workloads: []*kueue.Workload{
				utiltestingapi.MakeWorkload(testWorkload, testNamespace).PodSets(podSet(testPodSet, 2)).Obj(),
			},
			pods:           []*corev1.Pod{scheduled(pod("p1", testPodSet)).Obj()},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"workload with quota reservation but not admitted is skipped": {
			workloads: []*kueue.Workload{
				utiltestingapi.MakeWorkload(testWorkload, testNamespace).
					PodSets(podSet(testPodSet, 2)).
					ReserveQuotaAt(admission(podSet(testPodSet, 2)), earlier).
					Obj(),
			},
			pods:           []*corev1.Pod{scheduled(pod("p1", testPodSet)).Obj()},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"finished workload keeps its condition": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					FinishedAt(now).
					Condition(waitForScheduling(earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(earlier)),
			},
		},
		"admitted in the current second requeues after the settle period without writing": {
			workloads: []*kueue.Workload{
				admittedWorkloadAt(testWorkload, now, podSet(testPodSet, 1)).Obj(),
			},
			pods:           []*corev1.Pod{scheduled(pod("p1", testPodSet)).Obj()},
			wantResult:     reconcile.Result{RequeueAfter: time.Second},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"no pods and no condition leaves the workload untouched": {
			workloads:      []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"no pods and no condition with a zero grant leaves the workload untouched": {
			workloads:      []*kueue.Workload{admittedWorkload(podSet(testPodSet, 0)).Obj()},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"no pods and a current observation of all pods scheduled stands": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"no pods and a current observation of unscheduled pods stands": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now.Add(-30 * time.Second))),
			},
		},
		"no pods but a current condition and enough reclaimable pods report all pods scheduled": {
			features: map[featuregate.Feature]bool{features.ReclaimablePods: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					ReclaimablePods(kueue.ReclaimablePod{Name: testPodSet, Count: 2}).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"no pods and no condition with enough reclaimable pods leaves the workload untouched": {
			features: map[featuregate.Feature]bool{features.ReclaimablePods: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					ReclaimablePods(kueue.ReclaimablePod{Name: testPodSet, Count: 2}).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"only terminating pods do not open the observation": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				terminating(scheduled(pod("p1", testPodSet))).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"only terminating pods with a zero grant do not open the observation": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 0)).Obj()},
			pods: []*corev1.Pod{
				terminating(scheduled(pod("p1", testPodSet))).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"only failed pods of a previous admission do not open the observation": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).StatusPhase(corev1.PodFailed).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"only failed pods with a zero grant do not open the observation": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 0)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).StatusPhase(corev1.PodFailed).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"only failed pods with enough reclaimable pods do not open the observation": {
			features: map[featuregate.Feature]bool{features.ReclaimablePods: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					ReclaimablePods(kueue.ReclaimablePod{Name: testPodSet, Count: 1}).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).StatusPhase(corev1.PodFailed).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"succeeded pods below the grant do not open the observation": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).StatusPhase(corev1.PodSucceeded).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"succeeded pods filling the grant report all pods scheduled": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).StatusPhase(corev1.PodSucceeded).Obj(),
				pod("p2", testPodSet).StatusPhase(corev1.PodSucceeded).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"failed pod with a pending replacement reports unscheduled pods": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).StatusPhase(corev1.PodFailed).Obj(),
				pod("p1-replacement", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"pending pods are not scheduled": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).Obj(),
				pod("p2", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"some pods scheduled": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				pod("p2", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"all pods scheduled": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"pod bound with a preset nodeName counts as scheduled": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				pod("p1", testPodSet).NodeName("node-a").Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"failed pods do not satisfy the grant until replaced": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).StatusPhase(corev1.PodFailed).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"replacement pod scheduled after a failed one": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).StatusPhase(corev1.PodFailed).Obj(),
				scheduled(pod("p2-replacement", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"succeeded pods satisfy the grant": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				pod("p2", testPodSet).StatusPhase(corev1.PodSucceeded).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"terminating scheduled pod does not satisfy the grant": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				terminating(scheduled(pod("p2", testPodSet))).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"reclaimable pods satisfy the grant of deleted succeeded pods": {
			features: map[featuregate.Feature]bool{features.ReclaimablePods: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					ReclaimablePods(kueue.ReclaimablePod{Name: testPodSet, Count: 1}).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"reclaimable pods are ignored when the ReclaimablePods feature is disabled": {
			features: map[featuregate.Feature]bool{features.ReclaimablePods: false},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					ReclaimablePods(kueue.ReclaimablePod{Name: testPodSet, Count: 1}).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"reclaimable and succeeded pods are not summed": {
			features: map[featuregate.Feature]bool{features.ReclaimablePods: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 3)).
					ReclaimablePods(kueue.ReclaimablePod{Name: testPodSet, Count: 1}).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).StatusPhase(corev1.PodSucceeded).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"surplus scheduled pods are capped at the granted count": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
				scheduled(pod("p3", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"pods of another workload are ignored": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(testingpod.MakePod("other", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "other-wl").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
				pod("p1", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"pods of another podset do not satisfy the grant": {
			workloads: []*kueue.Workload{admittedWorkload(podSet("leader", 1), podSet("worker", 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("l1", "leader")).Obj(),
				scheduled(pod("w1", "worker")).Obj(),
				scheduled(pod("w-extra", "leader")).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"all podsets scheduled": {
			workloads: []*kueue.Workload{admittedWorkload(podSet("leader", 1), podSet("worker", 2)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("l1", "leader")).Obj(),
				scheduled(pod("w1", "worker")).Obj(),
				scheduled(pod("w2", "worker")).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"partial admission requires only the granted count": {
			workloads: []*kueue.Workload{
				utiltestingapi.MakeWorkload(testWorkload, testNamespace).
					PodSets(*utiltestingapi.MakePodSet(testPodSet, 3).SetMinimumCount(1).Request(corev1.ResourceCPU, "1").Obj()).
					ReserveQuotaAt(utiltestingapi.MakeAdmission("cq").PodSets(utiltestingapi.MakePodSetAssignment(testPodSet).Count(2).Obj()).Obj(), earlier).
					AdmittedAt(true, earlier).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"current observation of unscheduled pods stands while pods are still unscheduled": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				pod("p2", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now.Add(-30 * time.Second))),
			},
		},
		"progress without completion keeps the observation": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 3)).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
				pod("p3", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now.Add(-30 * time.Second))),
			},
		},
		"transition to all scheduled stamps the transition time": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"observation of all pods scheduled stands when a scheduled pod is deleted": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"generation change alone is not written": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Generation(2).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"observation records the generation": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Generation(2).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: func() *metav1.Condition {
					c := allScheduled(now)
					c.ObservedGeneration = 2
					return &c
				}(),
			},
		},
		"pod list failure keeps the previous condition and returns the error": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			listPodsErr: errList,
			wantErr:     errList,
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now.Add(-30 * time.Second))),
			},
		},
		"pod list failure with a condition of a previous admission keeps it and returns the error": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Condition(allScheduled(muchEarlier)).
					Obj(),
			},
			listPodsErr: errList,
			wantErr:     errList,
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(muchEarlier)),
			},
		},
		"missing workload is ignored": {
			request:        &types.NamespacedName{Namespace: testNamespace, Name: "missing"},
			workloads:      []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"pod carrying the workload UID is counted": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Annotation(kueue.WorkloadUIDAnnotation, string(testWorkloadUID)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"pod carrying another workload UID is ignored": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Annotation(kueue.WorkloadUIDAnnotation, "previous-uid").Obj(),
				pod("p2", testPodSet).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(waitForScheduling(now)),
			},
		},
		"only pods carrying another workload UID leave the workload untouched": {
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Annotation(kueue.WorkloadUIDAnnotation, "previous-uid").Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"elastic job: pod of the slice chain carrying another slice UID is counted": {
			features: map[featuregate.Feature]bool{features.ElasticJobsViaWorkloadSlices: true},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).
					Annotation(kueue.WorkloadSliceNameAnnotation, testWorkload).
					Annotation(kueue.WorkloadUIDAnnotation, "previous-slice-uid").
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"pod with a slice annotation carrying another UID is ignored by a non-elastic workload": {
			features:  map[featuregate.Feature]bool{features.ElasticJobsViaWorkloadSlices: true},
			workloads: []*kueue.Workload{admittedWorkload(podSet(testPodSet, 1)).Obj()},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).
					Annotation(kueue.WorkloadSliceNameAnnotation, testWorkload).
					Annotation(kueue.WorkloadUIDAnnotation, "previous-uid").
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"pod group: pods carrying any workload UID are counted": {
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					Annotation(podconstants.IsGroupWorkloadAnnotationKey, podconstants.IsGroupWorkloadAnnotationValue).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Annotation(kueue.WorkloadUIDAnnotation, "previous-uid").Obj(),
				scheduled(pod("p2", testPodSet)).Annotation(kueue.WorkloadUIDAnnotation, string(testWorkloadUID)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"MultiKueue admission check is ignored when the MultiKueue feature is disabled": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: false},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					AdmissionCheck(multiKueueState).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"admission check lookup failure is returned for retry": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					AdmissionCheck(multiKueueState).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			getAdmissionCheckErr: errGetAdmissionCheck,
			wantErr:              errGetAdmissionCheck,
			wantConditions:       map[string]*metav1.Condition{testWorkload: nil},
		},
		"workload with a MultiKueue admission check is not observed and drops its observation": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					AdmissionCheck(multiKueueState).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
				scheduled(pod("p2", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"workload with a MultiKueue admission check drops its observation with the merge patch": {
			features: map[featuregate.Feature]bool{
				features.MultiKueue:                   true,
				features.WorkloadRequestUseMergePatch: true,
			},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 2)).
					AdmissionCheck(multiKueueState).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"workload with a MultiKueue admission check drops its lifecycle reset": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				releasedWorkload(kueue.WorkloadOnHold, "The workload is on hold", podSet(testPodSet, 1)).
					AdmissionCheck(multiKueueState).
					Condition(lifecycle(kueue.WorkloadOnHold, "The workload is on hold", earlier)).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"workload with a MultiKueue admission check and no condition is left untouched": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					AdmissionCheck(multiKueueState).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"evicted workload with a MultiKueue admission check drops its condition instead of resetting it": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					AdmissionCheck(multiKueueState).
					Condition(evicted(kueue.WorkloadEvictedByPodsReadyTimeout, "Exceeded the PodsReady timeout")).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{testWorkload: nil},
		},
		"finished workload with a MultiKueue admission check keeps its condition": {
			features:        map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					AdmissionCheck(multiKueueState).
					FinishedAt(now).
					Condition(allScheduled(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now.Add(-30 * time.Second))),
			},
		},
		"workload with a non-MultiKueue admission check is observed": {
			features: map[featuregate.Feature]bool{features.MultiKueue: true},
			admissionChecks: []*kueue.AdmissionCheck{
				utiltestingapi.MakeAdmissionCheck("prov").ControllerName("example.com/provisioning").Obj(),
			},
			workloads: []*kueue.Workload{
				admittedWorkload(podSet(testPodSet, 1)).
					AdmissionCheck(kueue.AdmissionCheckState{Name: "prov", State: kueue.CheckStateReady}).
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(pod("p1", testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				testWorkload: new(allScheduled(now)),
			},
		},
		"elastic job: the finished origin slice redirects to the admitted slice": {
			features: map[featuregate.Feature]bool{features.ElasticJobsViaWorkloadSlices: true},
			request:  &types.NamespacedName{Namespace: testNamespace, Name: "wl-1"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-1", muchEarlier, podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					FinishedAt(earlier).
					Obj(),
				admittedWorkloadAt("wl-2", earlier, podSet(testPodSet, 2)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(testingpod.MakePod("p1", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-1").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
				scheduled(testingpod.MakePod("p2", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-2").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-1": nil,
				"wl-2": new(allScheduled(now)),
			},
		},
		"elastic job: the replaced slice still admitted redirects to the replacement": {
			features: map[featuregate.Feature]bool{features.ElasticJobsViaWorkloadSlices: true},
			request:  &types.NamespacedName{Namespace: testNamespace, Name: "wl-1"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-1", muchEarlier, podSet(testPodSet, 1)).
					UID("uid-1").
					Creation(muchEarlier).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Condition(allScheduled(muchEarlier.Add(30 * time.Second))).
					Obj(),
				admittedWorkloadAt("wl-2", earlier, podSet(testPodSet, 2)).
					UID("uid-2").
					Creation(earlier).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(testingpod.MakePod("p1", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-1").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
				testingpod.MakePod("p2", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-2").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Label(constants.PodSetLabel, string(testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-1": new(allScheduled(muchEarlier.Add(30 * time.Second))),
				"wl-2": new(waitForScheduling(now)),
			},
		},
		"elastic job: a deleted origin slice resolves to the admitted slice": {
			features: map[featuregate.Feature]bool{features.ElasticJobsViaWorkloadSlices: true},
			request:  &types.NamespacedName{Namespace: testNamespace, Name: "wl-0"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-2", earlier, podSet(testPodSet, 2)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-0").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(testingpod.MakePod("p1", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-0").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-0").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
				testingpod.MakePod("p2", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-2").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-0").
					Label(constants.PodSetLabel, string(testPodSet)).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-2": new(waitForScheduling(now)),
			},
		},
		"elastic job: a redirect to a variant slice is skipped": {
			features: map[featuregate.Feature]bool{
				features.ElasticJobsViaWorkloadSlices: true,
				features.ConcurrentAdmission:          true,
			},
			request: &types.NamespacedName{Namespace: testNamespace, Name: "wl-1"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-1", muchEarlier, podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					FinishedAt(earlier).
					Condition(allScheduled(muchEarlier.Add(30 * time.Second))).
					Obj(),
				admittedWorkloadAt("wl-2", earlier, podSet(testPodSet, 2)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					OwnerReference(kueue.SchemeGroupVersion.WithKind("Workload"), "parent", "parent-uid").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(testingpod.MakePod("p1", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-1").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-1": new(allScheduled(muchEarlier.Add(30 * time.Second))),
				"wl-2": nil,
			},
		},
		"elastic job: the evicted admitted slice is reset and not redirected": {
			features: map[featuregate.Feature]bool{features.ElasticJobsViaWorkloadSlices: true},
			request:  &types.NamespacedName{Namespace: testNamespace, Name: "wl-1"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-1", muchEarlier, podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Condition(evicted(kueue.WorkloadEvictedByPreemption, "Preempted")).
					Condition(allScheduled(muchEarlier.Add(30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-1": new(lifecycle(kueue.WorkloadEvictedByPreemption, "Preempted", now)),
			},
		},
		"elastic job: the MultiKueue check of the admitted slice drops the condition of the local origin slice request": {
			features: map[featuregate.Feature]bool{
				features.ElasticJobsViaWorkloadSlices: true,
				features.MultiKueue:                   true,
			},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			request:         &types.NamespacedName{Namespace: testNamespace, Name: "wl-1"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-1", muchEarlier, podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					FinishedAt(earlier).
					Condition(allScheduled(muchEarlier.Add(30 * time.Second))).
					Obj(),
				admittedWorkloadAt("wl-2", earlier, podSet(testPodSet, 2)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					AdmissionCheck(multiKueueState).
					Condition(waitForScheduling(now.Add(-30 * time.Second))).
					Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-1": new(allScheduled(muchEarlier.Add(30 * time.Second))),
				"wl-2": nil,
			},
		},
		"elastic job: the local admitted slice is observed on a request for the delegated origin slice": {
			features: map[featuregate.Feature]bool{
				features.ElasticJobsViaWorkloadSlices: true,
				features.MultiKueue:                   true,
			},
			admissionChecks: []*kueue.AdmissionCheck{multiKueueCheck},
			request:         &types.NamespacedName{Namespace: testNamespace, Name: "wl-1"},
			workloads: []*kueue.Workload{
				admittedWorkloadAt("wl-1", muchEarlier, podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					AdmissionCheck(multiKueueState).
					FinishedAt(earlier).
					Obj(),
				admittedWorkloadAt("wl-2", earlier, podSet(testPodSet, 1)).
					Annotation(workloadslicing.EnabledAnnotationKey, workloadslicing.EnabledAnnotationValue).
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Obj(),
			},
			pods: []*corev1.Pod{
				scheduled(testingpod.MakePod("p1", testNamespace).
					Annotation(kueue.WorkloadAnnotation, "wl-2").
					Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").
					Label(constants.PodSetLabel, string(testPodSet))).Obj(),
			},
			wantConditions: map[string]*metav1.Condition{
				"wl-1": nil,
				"wl-2": new(allScheduled(now)),
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			for feature, enabled := range tc.features {
				features.SetFeatureGateDuringTest(t, feature, enabled)
			}
			ctx, _ := utiltesting.ContextWithLog(t)
			interceptorFuncs := interceptor.Funcs{SubResourcePatch: utiltesting.TreatSSAAsStrategicMerge}
			if tc.listPodsErr != nil {
				interceptorFuncs.List = func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, isPodList := list.(*corev1.PodList); isPodList {
						return tc.listPodsErr
					}
					return c.List(ctx, list, opts...)
				}
			}
			if tc.getAdmissionCheckErr != nil {
				interceptorFuncs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isAdmissionCheck := obj.(*kueue.AdmissionCheck); isAdmissionCheck {
						return tc.getAdmissionCheckErr
					}
					return c.Get(ctx, key, obj, opts...)
				}
			}
			if tc.updateBeforePatch != nil {
				updated := false
				interceptorFuncs.SubResourcePatch = func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if !updated {
						updated = true
						live := &kueue.Workload{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
							return err
						}
						tc.updateBeforePatch(live)
						if err := c.Status().Update(ctx, live); err != nil {
							return err
						}
					}
					return utiltesting.TreatSSAAsStrategicMerge(ctx, c, subResourceName, obj, patch, opts...)
				}
			}
			clientBuilder := utiltesting.NewClientBuilder().
				WithInterceptorFuncs(interceptorFuncs).
				WithIndex(&corev1.Pod{}, indexer.WorkloadSliceNameKey, indexer.IndexPodWorkloadSliceName).
				WithIndex(&kueue.Workload{}, indexer.WorkloadSliceNameKey, indexer.IndexWorkloadSliceName)
			for _, p := range tc.pods {
				clientBuilder = clientBuilder.WithObjects(p)
			}
			for _, ac := range tc.admissionChecks {
				clientBuilder = clientBuilder.WithObjects(ac)
			}
			for _, wl := range tc.workloads {
				clientBuilder = clientBuilder.WithStatusSubresource(wl)
			}
			kClient := clientBuilder.Build()
			for _, wl := range tc.workloads {
				if err := kClient.Create(ctx, wl); err != nil {
					t.Fatalf("Could not create workload %s: %v", wl.Name, err)
				}
			}

			request := types.NamespacedName{Namespace: testNamespace, Name: testWorkload}
			if tc.request != nil {
				request = *tc.request
			}
			tracker := NewTracker(kClient, nil, withClock(testingclock.NewFakeClock(now)))
			gotResult, gotErr := tracker.Reconcile(ctx, reconcile.Request{NamespacedName: request})
			if tc.wantConflict {
				if !apierrors.IsConflict(gotErr) {
					t.Errorf("Reconcile returned %v, want a conflict error", gotErr)
				}
			} else if diff := cmp.Diff(tc.wantErr, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Reconcile returned unexpected error (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantResult, gotResult); diff != "" {
				t.Errorf("Reconcile returned unexpected result (-want,+got):\n%s", diff)
			}
			if tc.reconcileAgain {
				gotResult, gotErr := tracker.Reconcile(ctx, reconcile.Request{NamespacedName: request})
				if gotErr != nil {
					t.Errorf("Second reconcile returned unexpected error: %v", gotErr)
				}
				if diff := cmp.Diff(tc.wantSecondResult, gotResult); diff != "" {
					t.Errorf("Second reconcile returned unexpected result (-want,+got):\n%s", diff)
				}
			}

			for wlName, wantCondition := range tc.wantConditions {
				gotWorkload := &kueue.Workload{}
				if err := kClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: wlName}, gotWorkload); err != nil {
					t.Fatalf("Could not get workload %s: %v", wlName, err)
				}
				gotCondition := apimeta.FindStatusCondition(gotWorkload.Status.Conditions, kueue.WorkloadPodsScheduled)
				if diff := cmp.Diff(wantCondition, gotCondition); diff != "" {
					t.Errorf("Unexpected PodsScheduled condition on workload %s (-want,+got):\n%s", wlName, diff)
				}
			}
		})
	}
}

func TestPodHandler(t *testing.T) {
	now := time.Now()
	pod := func(name string) *testingpod.PodWrapper {
		return testingpod.MakePod(name, testNamespace)
	}
	linked := func(name, workload string) *testingpod.PodWrapper {
		return pod(name).Annotation(kueue.WorkloadAnnotation, workload).Label(constants.PodSetLabel, string(testPodSet))
	}
	request := func(workload string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: workload}}
	}

	testCases := map[string]struct {
		handle       func(context.Context, *podHandler, workqueue.TypedRateLimitingInterface[reconcile.Request])
		wantRequests []reconcile.Request
	}{
		"create of a linked pod enqueues its workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Create(ctx, event.CreateEvent{Object: linked("p1", testWorkload).Obj()}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"create of a pod without workload annotations is ignored": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Create(ctx, event.CreateEvent{Object: pod("p1").Label(constants.PodSetLabel, string(testPodSet)).Obj()}, q)
			},
		},
		"the workload slice name annotation takes precedence over the workload annotation": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Create(ctx, event.CreateEvent{Object: linked("p1", "wl-2").Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").Obj()}, q)
			},
			wantRequests: []reconcile.Request{request("wl-1")},
		},
		"delete of a linked pod enqueues its workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Delete(ctx, event.DeleteEvent{Object: linked("p1", testWorkload).Obj()}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update binding the pod enqueues its workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).Obj(),
					ObjectNew: linked("p1", testWorkload).NodeName("node-a").Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update changing the phase enqueues its workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).NodeName("node-a").Obj(),
					ObjectNew: linked("p1", testWorkload).NodeName("node-a").StatusPhase(corev1.PodSucceeded).Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update starting the deletion enqueues its workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).NodeName("node-a").Obj(),
					ObjectNew: linked("p1", testWorkload).NodeName("node-a").DeletionTimestamp(now).Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update without a scheduling change is ignored": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).NodeName("node-a").Obj(),
					ObjectNew: linked("p1", testWorkload).NodeName("node-a").Label("extra", "label").Obj(),
				}, q)
			},
		},
		"update relinking the pod enqueues both workloads": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", "wl-a").Obj(),
					ObjectNew: linked("p1", "wl-b").Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request("wl-a"), request("wl-b")},
		},
		"update unlinking the pod enqueues the previous workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", "wl-a").Obj(),
					ObjectNew: pod("p1").Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request("wl-a")},
		},
		"update linking the pod enqueues the new workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: pod("p1").Obj(),
					ObjectNew: linked("p1", testWorkload).Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update adding the workload UID annotation enqueues the workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).Obj(),
					ObjectNew: linked("p1", testWorkload).Annotation(kueue.WorkloadUIDAnnotation, "uid-1").Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update changing the workload UID annotation enqueues the workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).Annotation(kueue.WorkloadUIDAnnotation, "uid-1").Obj(),
					ObjectNew: linked("p1", testWorkload).Annotation(kueue.WorkloadUIDAnnotation, "uid-2").Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update removing the workload UID annotation enqueues the workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).Annotation(kueue.WorkloadUIDAnnotation, "uid-1").Obj(),
					ObjectNew: linked("p1", testWorkload).Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update changing the podset label enqueues the workload once": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).Obj(),
					ObjectNew: linked("p1", testWorkload).Label(constants.PodSetLabel, "worker").Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"update setting the PodScheduled condition enqueues the workload": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Update(ctx, event.UpdateEvent{
					ObjectOld: linked("p1", testWorkload).Obj(),
					ObjectNew: linked("p1", testWorkload).StatusConditions(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).Obj(),
				}, q)
			},
			wantRequests: []reconcile.Request{request(testWorkload)},
		},
		"create of a pod with only the workload slice name annotation enqueues the slice": {
			handle: func(ctx context.Context, h *podHandler, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				h.Create(ctx, event.CreateEvent{Object: pod("p1").Annotation(kueue.WorkloadSliceNameAnnotation, "wl-1").Obj()}, q)
			},
			wantRequests: []reconcile.Request{request("wl-1")},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := utiltesting.ContextWithLog(t)
			q := &immediateQueue{TypedInterface: workqueue.NewTyped[reconcile.Request]()}
			tc.handle(ctx, &podHandler{}, q)
			var gotRequests []reconcile.Request
			for q.Len() > 0 {
				item, _ := q.Get()
				gotRequests = append(gotRequests, item)
				q.Done(item)
			}
			slices.SortFunc(gotRequests, func(a, b reconcile.Request) int {
				return strings.Compare(a.String(), b.String())
			})
			if diff := cmp.Diff(tc.wantRequests, gotRequests, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected requests (-want,+got):\n%s", diff)
			}
		})
	}
}

type immediateQueue struct {
	workqueue.TypedInterface[reconcile.Request]
}

var _ workqueue.TypedRateLimitingInterface[reconcile.Request] = (*immediateQueue)(nil)

func (q *immediateQueue) AddAfter(item reconcile.Request, _ time.Duration) {
	q.Add(item)
}

func (q *immediateQueue) AddRateLimited(item reconcile.Request) {
	q.Add(item)
}

func (q *immediateQueue) Forget(reconcile.Request) {}

func (q *immediateQueue) NumRequeues(reconcile.Request) int {
	return 0
}

func TestWorkloadPredicates(t *testing.T) {
	now := time.Now()
	admission := utiltestingapi.MakeAdmission("cq").Obj()
	podsScheduled := metav1.Condition{Type: kueue.WorkloadPodsScheduled, Status: metav1.ConditionTrue, Reason: kueue.WorkloadAllRequiredPodsScheduled}
	admitted := utiltestingapi.MakeWorkload(testWorkload, testNamespace).ReserveQuotaAt(admission, now).AdmittedAt(true, now).Obj()
	pending := utiltestingapi.MakeWorkload(testWorkload, testNamespace).Obj()
	pendingWithCondition := utiltestingapi.MakeWorkload(testWorkload, testNamespace).Condition(podsScheduled).Obj()
	evicted := utiltestingapi.MakeWorkload(testWorkload, testNamespace).ReserveQuotaAt(admission, now).AdmittedAt(true, now).EvictedAt(now).Obj()
	evictedWithCondition := utiltestingapi.MakeWorkload(testWorkload, testNamespace).ReserveQuotaAt(admission, now).AdmittedAt(true, now).EvictedAt(now).Condition(podsScheduled).Obj()
	finished := utiltestingapi.MakeWorkload(testWorkload, testNamespace).ReserveQuotaAt(admission, now).AdmittedAt(true, now).FinishedAt(now).Obj()
	finishedWithCondition := utiltestingapi.MakeWorkload(testWorkload, testNamespace).ReserveQuotaAt(admission, now).AdmittedAt(true, now).FinishedAt(now).Condition(podsScheduled).Obj()

	tracker := &Tracker{}
	testCases := map[string]struct {
		got  bool
		want bool
	}{
		"create of an admitted workload":                      {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: admitted}), want: true},
		"create of a pending workload":                        {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: pending})},
		"create of a pending workload with a condition":       {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: pendingWithCondition}), want: true},
		"create of an evicted workload":                       {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: evicted})},
		"create of an evicted workload with a condition":      {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: evictedWithCondition}), want: true},
		"create of a finished workload":                       {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: finished})},
		"create of a finished workload with a condition":      {got: tracker.Create(event.TypedCreateEvent[*kueue.Workload]{Object: finishedWithCondition}), want: true},
		"update to an admitted workload":                      {got: tracker.Update(event.TypedUpdateEvent[*kueue.Workload]{ObjectOld: pending, ObjectNew: admitted}), want: true},
		"update to a pending workload":                        {got: tracker.Update(event.TypedUpdateEvent[*kueue.Workload]{ObjectOld: admitted, ObjectNew: pending})},
		"update to a pending workload keeping its condition":  {got: tracker.Update(event.TypedUpdateEvent[*kueue.Workload]{ObjectOld: admitted, ObjectNew: pendingWithCondition}), want: true},
		"update to an evicted workload":                       {got: tracker.Update(event.TypedUpdateEvent[*kueue.Workload]{ObjectOld: admitted, ObjectNew: evicted})},
		"update to an evicted workload keeping its condition": {got: tracker.Update(event.TypedUpdateEvent[*kueue.Workload]{ObjectOld: admitted, ObjectNew: evictedWithCondition}), want: true},
		"delete":  {got: tracker.Delete(event.TypedDeleteEvent[*kueue.Workload]{Object: admitted})},
		"generic": {got: tracker.Generic(event.TypedGenericEvent[*kueue.Workload]{Object: admitted})},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("predicate = %t, want %t", tc.got, tc.want)
			}
		})
	}
}
