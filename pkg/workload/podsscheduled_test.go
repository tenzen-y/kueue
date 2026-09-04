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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

func TestCurrentPodsScheduledCondition(t *testing.T) {
	admittedAt := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	later := admittedAt.Add(time.Second)

	podsScheduled := func(status metav1.ConditionStatus, reason string, transition time.Time, generation int64) metav1.Condition {
		return metav1.Condition{
			Type:               kueue.WorkloadPodsScheduled,
			Status:             status,
			Reason:             reason,
			ObservedGeneration: generation,
			LastTransitionTime: metav1.NewTime(transition),
		}
	}

	testCases := map[string]struct {
		generation int64
		conditions []metav1.Condition
		want       *metav1.Condition
	}{
		"no condition": {
			generation: 1,
		},
		"other conditions only": {
			generation: 1,
			conditions: []metav1.Condition{{Type: kueue.WorkloadPodsReady, Status: metav1.ConditionFalse, Reason: kueue.WorkloadWaitForStart}},
		},
		"false and waiting for scheduling, observed after the admission": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, later, 1)},
			want:       new(podsScheduled(metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, later, 1)),
		},
		"true and all required pods scheduled, observed after the admission": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, later, 1)},
			want:       new(podsScheduled(metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, later, 1)),
		},
		"transitioned before the admission": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, admittedAt.Add(-time.Second), 1)},
		},
		"transitioned in the same second as the admission": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionTrue, kueue.WorkloadAllRequiredPodsScheduled, admittedAt, 1)},
		},
		"an observation of an older generation is still valid": {
			generation: 2,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, later, 1)},
			want:       new(podsScheduled(metav1.ConditionFalse, kueue.WorkloadWaitForScheduling, later, 1)),
		},
		"unknown status": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionUnknown, kueue.WorkloadWaitForScheduling, later, 1)},
		},
		"false with an unexpected reason": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionFalse, "SomethingElse", later, 1)},
		},
		"true with the reason of the false status": {
			generation: 1,
			conditions: []metav1.Condition{podsScheduled(metav1.ConditionTrue, kueue.WorkloadWaitForScheduling, later, 1)},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			wl := &kueue.Workload{
				ObjectMeta: metav1.ObjectMeta{Generation: tc.generation},
				Status:     kueue.WorkloadStatus{Conditions: tc.conditions},
			}
			got := CurrentPodsScheduledCondition(wl, admittedAt)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Unexpected condition (-want,+got):\n%s", diff)
			}
		})
	}
}
