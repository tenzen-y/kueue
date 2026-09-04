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

package workload

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	workloadpatching "sigs.k8s.io/kueue/pkg/workload/patching"
)

func TestUnsetQuotaReservationWithConditionResetsPodsReady(t *testing.T) {
	admittedAt := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	now := admittedAt.Add(time.Minute)

	podsReady := func(status metav1.ConditionStatus, reason, message string, transition time.Time) *metav1.Condition {
		return &metav1.Condition{
			Type:               kueue.WorkloadPodsReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: 1,
			LastTransitionTime: metav1.NewTime(transition),
		}
	}
	started := podsReady(metav1.ConditionTrue, kueue.WorkloadStarted, "All pods reached readiness and the workload is running", admittedAt)
	waitForStart := podsReady(metav1.ConditionFalse, kueue.WorkloadWaitForStart, PodsNotReadyMessage, now)
	waitForStartSinceAdmission := podsReady(metav1.ConditionFalse, kueue.WorkloadWaitForStart, PodsNotReadyMessage, admittedAt)

	testCases := map[string]struct {
		disableWaitForPodsReady bool
		concurrentAdmission     bool
		variant                 bool
		quotaReservedOnly       bool
		podsReady               *metav1.Condition
		wantPodsReady           *metav1.Condition
	}{
		"DisableWaitForPodsReady on: untouched": {
			disableWaitForPodsReady: true,
			podsReady:               started,
			wantPodsReady:           started,
		},
		"ConcurrentAdmission on and variant: untouched": {
			concurrentAdmission: true,
			variant:             true,
			podsReady:           started,
			wantPodsReady:       started,
		},
		"ConcurrentAdmission off and variant: reset": {
			variant:       true,
			podsReady:     started,
			wantPodsReady: waitForStart,
		},
		"quota reserved but not admitted: untouched": {
			quotaReservedOnly: true,
			podsReady:         started,
			wantPodsReady:     started,
		},
		"PodsReady absent: stays absent": {},
		"true and started: reset to wait for start": {
			podsReady:     started,
			wantPodsReady: waitForStart,
		},
		"false and waiting for recovery: reset to wait for start": {
			podsReady:     podsReady(metav1.ConditionFalse, kueue.WorkloadWaitForRecovery, "At least one pod has failed, waiting for recovery", admittedAt),
			wantPodsReady: waitForStartSinceAdmission,
		},
		"false and waiting for scheduling: reset to wait for start": {
			podsReady:     podsReady(metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, "Not all pods are scheduled", admittedAt),
			wantPodsReady: waitForStartSinceAdmission,
		},
		"false and waiting for start with a different message: unchanged": {
			podsReady:     podsReady(metav1.ConditionFalse, kueue.WorkloadWaitForStart, "custom message", admittedAt),
			wantPodsReady: podsReady(metav1.ConditionFalse, kueue.WorkloadWaitForStart, "custom message", admittedAt),
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.DisableWaitForPodsReady, tc.disableWaitForPodsReady)
			features.SetFeatureGateDuringTest(t, features.ConcurrentAdmission, tc.concurrentAdmission)

			wlWrapper := utiltestingapi.MakeWorkload("wl", "ns").
				Generation(1).
				ReserveQuotaAt(utiltestingapi.MakeAdmission("cq").Obj(), admittedAt)
			if !tc.quotaReservedOnly {
				wlWrapper.AdmittedAt(true, admittedAt)
			}
			if tc.variant {
				wlWrapper.OwnerReference(kueue.SchemeGroupVersion.WithKind("Workload"), "parent", "parent-uid")
			}
			if tc.podsReady != nil {
				wlWrapper.Condition(*tc.podsReady)
			}
			wl := wlWrapper.Obj()

			UnsetQuotaReservationWithCondition(wl, "Pending", "released", now)

			got := apimeta.FindStatusCondition(wl.Status.Conditions, kueue.WorkloadPodsReady)
			if diff := cmp.Diff(tc.wantPodsReady, got); diff != "" {
				t.Errorf("Unexpected PodsReady condition (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestUnsetQuotaReservationWithConditionPatchesPodsReady(t *testing.T) {
	admittedAt := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	now := admittedAt.Add(time.Minute)
	fakeClock := testingclock.NewFakeClock(now)

	testCases := map[string]struct {
		useMergePatch bool
	}{
		"server-side apply": {},
		"merge patch": {
			useMergePatch: true,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.WorkloadRequestUseMergePatch, tc.useMergePatch)

			ctx, _ := utiltesting.ContextWithLog(t)

			wl := utiltestingapi.MakeWorkload("wl", "ns").
				Generation(1).
				ReserveQuotaAt(utiltestingapi.MakeAdmission("cq").Obj(), admittedAt).
				AdmittedAt(true, admittedAt).
				Condition(metav1.Condition{
					Type:               kueue.WorkloadPodsReady,
					Status:             metav1.ConditionTrue,
					Reason:             kueue.WorkloadStarted,
					Message:            "All pods reached readiness and the workload is running",
					ObservedGeneration: 1,
					LastTransitionTime: metav1.NewTime(admittedAt),
				}).
				Obj()

			cl := utiltesting.NewClientBuilder().
				WithObjects(wl).
				WithStatusSubresource(&kueue.Workload{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: utiltesting.TreatSSAAsStrategicMerge,
				}).
				Build()

			err := workloadpatching.PatchAdmissionStatus(ctx, cl, wl, fakeClock, func(wl *kueue.Workload) (bool, error) {
				return UnsetQuotaReservationWithCondition(wl, "Pending", "released", now), nil
			})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			var updatedWl kueue.Workload
			if err := cl.Get(ctx, client.ObjectKeyFromObject(wl), &updatedWl); err != nil {
				t.Fatalf("Failed obtaining updated object: %v", err)
			}

			wantPodsReady := &metav1.Condition{
				Type:               kueue.WorkloadPodsReady,
				Status:             metav1.ConditionFalse,
				Reason:             kueue.WorkloadWaitForStart,
				Message:            PodsNotReadyMessage,
				ObservedGeneration: 1,
			}
			got := apimeta.FindStatusCondition(updatedWl.Status.Conditions, kueue.WorkloadPodsReady)
			if diff := cmp.Diff(wantPodsReady, got, cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime")); diff != "" {
				t.Errorf("Unexpected persisted PodsReady condition (-want,+got):\n%s", diff)
			}
		})
	}
}
