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

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workloadslicing"
)

func TestSanitizePodSets(t *testing.T) {
	testCases := map[string]struct {
		podSets         []kueue.PodSet
		expectedPodSets []kueue.PodSet
	}{
		"init containers and containers": {
			podSets: []kueue.PodSet{
				*utiltestingapi.MakePodSet("test", 1).
					Containers(*utiltesting.MakeContainer().
						Name("c1").
						WithEnvVar(corev1.EnvVar{Name: "ENV1", Value: "value1"}).
						WithEnvVar(corev1.EnvVar{Name: "ENV1", Value: "value2"}).
						Obj()).
					InitContainers(*utiltesting.MakeContainer().
						Name("init1").
						WithEnvVar(corev1.EnvVar{Name: "ENV2", Value: "value3"}).
						WithEnvVar(corev1.EnvVar{Name: "ENV2", Value: "value4"}).
						Obj()).
					Obj(),
			},
			expectedPodSets: []kueue.PodSet{
				*utiltestingapi.MakePodSet("test", 1).
					Containers(*utiltesting.MakeContainer().
						Name("c1").
						WithEnvVar(corev1.EnvVar{Name: "ENV1", Value: "value2"}).
						Obj()).
					InitContainers(*utiltesting.MakeContainer().
						Name("init1").
						WithEnvVar(corev1.EnvVar{Name: "ENV2", Value: "value4"}).
						Obj()).
					Obj(),
			},
		},
		"containers only": {
			podSets: []kueue.PodSet{
				*utiltestingapi.MakePodSet("test", 1).
					Containers(*utiltesting.MakeContainer().
						Name("c1").
						WithEnvVar(corev1.EnvVar{Name: "ENV1", Value: "value1"}).
						WithEnvVar(corev1.EnvVar{Name: "ENV1", Value: "value2"}).
						Obj()).
					Obj(),
			},
			expectedPodSets: []kueue.PodSet{
				*utiltestingapi.MakePodSet("test", 1).
					Containers(*utiltesting.MakeContainer().
						Name("c1").
						WithEnvVar(corev1.EnvVar{Name: "ENV1", Value: "value2"}).
						Obj()).
					Obj(),
			},
		},
		"empty podsets": {
			podSets:         []kueue.PodSet{},
			expectedPodSets: []kueue.PodSet{},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			SanitizePodSets(tc.podSets)

			if diff := cmp.Diff(tc.expectedPodSets, tc.podSets); diff != "" {
				t.Errorf("unexpected difference: %s", diff)
			}
		})
	}
}

func TestComparePodSetCounts(t *testing.T) {
	testCases := map[string]struct {
		podSets         []kueue.PodSet
		referenceCounts map[kueue.PodSetReference]int32
		wantChanged     bool
	}{
		"equal counts": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 3},
			},
			referenceCounts: map[kueue.PodSetReference]int32{
				"head":   1,
				"worker": 3,
			},
			wantChanged: false,
		},
		"different count": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 5},
			},
			referenceCounts: map[kueue.PodSetReference]int32{
				"head":   1,
				"worker": 3,
			},
			wantChanged: true,
		},
		"different length": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 3},
			},
			referenceCounts: map[kueue.PodSetReference]int32{
				"head": 1,
			},
			wantChanged: true,
		},
		"missing podset in reference": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 3},
			},
			referenceCounts: map[kueue.PodSetReference]int32{
				"head":    1,
				"worker2": 3,
			},
			wantChanged: true,
		},
		"empty reference": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
			},
			referenceCounts: map[kueue.PodSetReference]int32{},
			wantChanged:     true,
		},
		"both empty": {
			podSets:         []kueue.PodSet{},
			referenceCounts: map[kueue.PodSetReference]int32{},
			wantChanged:     false,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got := ComparePodSetCounts(tc.podSets, tc.referenceCounts)
			if got != tc.wantChanged {
				t.Errorf("ComparePodSetCounts() = %v, want %v", got, tc.wantChanged)
			}
		})
	}
}

func TestParsePodSetReplicaSizes(t *testing.T) {
	testCases := map[string]struct {
		annotation string
		wantCounts map[kueue.PodSetReference]int32
		wantErr    bool
	}{
		"empty annotation": {
			annotation: "",
			wantCounts: map[kueue.PodSetReference]int32{},
		},
		"valid annotation": {
			annotation: `[{"name":"head","count":1},{"name":"worker","count":3}]`,
			wantCounts: map[kueue.PodSetReference]int32{
				"head":   1,
				"worker": 3,
			},
		},
		"single podset": {
			annotation: `[{"name":"head","count":1}]`,
			wantCounts: map[kueue.PodSetReference]int32{
				"head": 1,
			},
		},
		"invalid json": {
			annotation: `invalid`,
			wantErr:    true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := ParsePodSetReplicaSizes(tc.annotation)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParsePodSetReplicaSizes() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if diff := cmp.Diff(tc.wantCounts, got); diff != "" {
					t.Errorf("ParsePodSetReplicaSizes() mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestSerializePodSetCounts(t *testing.T) {
	testCases := map[string]struct {
		podSets  []kueue.PodSet
		wantJSON string
	}{
		"single podset": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
			},
			wantJSON: `[{"name":"head","count":1}]`,
		},
		"multiple podsets": {
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 5},
			},
			wantJSON: `[{"name":"head","count":1},{"name":"worker","count":5}]`,
		},
		"empty podsets": {
			podSets:  []kueue.PodSet{},
			wantJSON: `[]`,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := SerializePodSetCounts(tc.podSets)
			if err != nil {
				t.Fatalf("SerializePodSetCounts() unexpected error: %v", err)
			}
			if string(got) != tc.wantJSON {
				t.Errorf("SerializePodSetCounts() = %s, want %s", string(got), tc.wantJSON)
			}
		})
	}
}

func TestGetWorkloadslicingCustomAnnotations(t *testing.T) {
	testCases := map[string]struct {
		annotations    map[string]string
		podSets        []kueue.PodSet
		wantAnnotation map[string]string
	}{
		"workload slicing disabled returns nil": {
			annotations: map[string]string{},
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
			},
			wantAnnotation: nil,
		},
		"first call sets annotation": {
			annotations: map[string]string{
				workloadslicing.EnabledAnnotationKey: workloadslicing.EnabledAnnotationValue,
			},
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 3},
			},
			wantAnnotation: map[string]string{
				PodsetReplicaSizesAnnotation: `[{"name":"head","count":1},{"name":"worker","count":3}]`,
			},
		},
		"no change when annotation matches": {
			annotations: map[string]string{
				workloadslicing.EnabledAnnotationKey: workloadslicing.EnabledAnnotationValue,
				PodsetReplicaSizesAnnotation:         `[{"name":"head","count":1},{"name":"worker","count":3}]`,
			},
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 3},
			},
			wantAnnotation: nil,
		},
		"updated when counts differ": {
			annotations: map[string]string{
				workloadslicing.EnabledAnnotationKey: workloadslicing.EnabledAnnotationValue,
				PodsetReplicaSizesAnnotation:         `[{"name":"head","count":1},{"name":"worker","count":3}]`,
			},
			podSets: []kueue.PodSet{
				{Name: "head", Count: 1},
				{Name: "worker", Count: 5},
			},
			wantAnnotation: map[string]string{
				PodsetReplicaSizesAnnotation: `[{"name":"head","count":1},{"name":"worker","count":5}]`,
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.ElasticJobsViaWorkloadSlices, true)

			obj := &metav1.ObjectMeta{
				Name:        "test-object",
				Namespace:   "default",
				Annotations: tc.annotations,
			}
			// Use a corev1.ConfigMap as a simple client.Object wrapper
			cm := &corev1.ConfigMap{ObjectMeta: *obj}

			got, err := GetWorkloadslicingCustomAnnotations(cm, tc.podSets)
			if err != nil {
				t.Fatalf("GetWorkloadslicingCustomAnnotations() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantAnnotation, got); diff != "" {
				t.Errorf("GetWorkloadslicingCustomAnnotations() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
