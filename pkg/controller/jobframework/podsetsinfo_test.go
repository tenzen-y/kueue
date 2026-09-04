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

package jobframework

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-base/featuregate"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
)

func TestGetPodSetsInfoFromStatusWorkloadAnnotations(t *testing.T) {
	testCases := map[string]struct {
		features         map[featuregate.Feature]bool
		annotateWorkload bool
		wantAnnotations  map[string]string
	}{
		"waitForPodsReady enabled injects the workload name and UID": {
			features: map[featuregate.Feature]bool{
				features.TopologyAwareScheduling:     false,
				features.SchedulerLibraryIntegration: false,
			},
			annotateWorkload: true,
			wantAnnotations: map[string]string{
				kueue.WorkloadAnnotation:    "wl",
				kueue.WorkloadUIDAnnotation: "wl-uid",
			},
		},
		"waitForPodsReady disabled and no feature needing the annotations": {
			features: map[featuregate.Feature]bool{
				features.TopologyAwareScheduling:     false,
				features.SchedulerLibraryIntegration: false,
			},
		},
		"TopologyAwareScheduling injects the workload name only": {
			features: map[featuregate.Feature]bool{
				features.TopologyAwareScheduling:     true,
				features.SchedulerLibraryIntegration: false,
			},
			wantAnnotations: map[string]string{kueue.WorkloadAnnotation: "wl"},
		},
		"SchedulerLibraryIntegration injects the workload name only": {
			features: map[featuregate.Feature]bool{
				features.TopologyAwareScheduling:     false,
				features.SchedulerLibraryIntegration: true,
			},
			wantAnnotations: map[string]string{kueue.WorkloadAnnotation: "wl"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGatesDuringTest(t, tc.features)
			ctx, _ := utiltesting.ContextWithLog(t)
			wl := utiltestingapi.MakeWorkload("wl", "ns").
				UID("wl-uid").
				PodSets(*utiltestingapi.MakePodSet("main", 1).Request(corev1.ResourceCPU, "1").Obj()).
				ReserveQuotaAt(utiltestingapi.MakeAdmission("cq").PodSets(utiltestingapi.MakePodSetAssignment("main").Count(1).Obj()).Obj(), time.Now()).
				Obj()
			got, err := getPodSetsInfoFromStatus(ctx, utiltesting.NewFakeClient(), wl, tc.annotateWorkload)
			if err != nil {
				t.Fatalf("getPodSetsInfoFromStatus() returned an unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("getPodSetsInfoFromStatus() returned %d PodSetInfos, want 1", len(got))
			}
			if diff := cmp.Diff(tc.wantAnnotations, got[0].Annotations, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("unexpected annotations (-want,+got):\n%s", diff)
			}
		})
	}
}
