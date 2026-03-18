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
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/util/orderedgroups"
	"sigs.k8s.io/kueue/pkg/workloadslicing"
)

const (
	// PodsetReplicaSizesAnnotation is set on the job when autoscaling causes
	// PodSet replica sizes to differ from the original spec. The value is a JSON
	// array compatible with []kueue.PodSet, containing only the changed PodSets.
	// This annotation is alpha-level enabled by the ElasticJobsViaWorkloadSlices.
	PodsetReplicaSizesAnnotation = "kueue.x-k8s.io/podset-replica-sizes"
)

// PodSetReplicaSize is a minimal representation of a PodSet for the
// PodsetReplicaSizesAnnotation, containing only name and count.
type PodSetReplicaSize struct {
	Name  kueue.PodSetReference `json:"name"`
	Count int32                 `json:"count"`
}

// JobPodSets retrieves the pod sets from a GenericJob and applies environment variable
// deduplication.
func JobPodSets(ctx context.Context, job GenericJob) ([]kueue.PodSet, error) {
	podSets, err := job.PodSets(ctx)
	if err != nil {
		return nil, err
	}
	SanitizePodSets(podSets)
	return podSets, nil
}

// SanitizePodSets sanitizes all PodSets in the given slice by removing duplicate
// environment variables from each container. This function modifies the podSets slice in place.
func SanitizePodSets(podSets []kueue.PodSet) {
	for podSetIndex := range podSets {
		SanitizePodSet(&podSets[podSetIndex])
	}
}

// SanitizePodSet sanitizes a single PodSet by removing duplicate environment
// variables from all containers and initContainers in its pod template.
func SanitizePodSet(podSet *kueue.PodSet) {
	for containerIndex := range podSet.Template.Spec.Containers {
		sanitizeContainer(&podSet.Template.Spec.Containers[containerIndex])
	}

	for containerIndex := range podSet.Template.Spec.InitContainers {
		sanitizeContainer(&podSet.Template.Spec.InitContainers[containerIndex])
	}
}

// sanitizeContainer removes duplicate environment variables from the given container.
func sanitizeContainer(container *corev1.Container) {
	envVarGroups := orderedgroups.NewOrderedGroups[string, corev1.EnvVar]()
	for _, envVar := range container.Env {
		envVarGroups.Insert(envVar.Name, envVar)
	}
	container.Env = make([]corev1.EnvVar, 0, len(container.Env))
	for _, envVars := range envVarGroups.InOrder {
		container.Env = append(container.Env, envVars[len(envVars)-1])
	}
}

// ComparePodSetCounts returns true if any PodSet count differs from referenceCounts.
func ComparePodSetCounts(podSets []kueue.PodSet, referenceCounts map[kueue.PodSetReference]int32) bool {
	if len(podSets) != len(referenceCounts) {
		return true
	}
	for _, ps := range podSets {
		if refCount, ok := referenceCounts[ps.Name]; !ok || ps.Count != refCount {
			return true
		}
	}
	return false
}

// ParsePodSetReplicaSizes parses the PodsetReplicaSizesAnnotation value into a map.
// Returns an empty map if the annotation is absent or empty.
func ParsePodSetReplicaSizes(annotation string) (map[kueue.PodSetReference]int32, error) {
	counts := make(map[kueue.PodSetReference]int32)
	if annotation == "" {
		return counts, nil
	}
	var podSets []PodSetReplicaSize
	if err := json.Unmarshal([]byte(annotation), &podSets); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s annotation: %w", PodsetReplicaSizesAnnotation, err)
	}
	for _, ps := range podSets {
		counts[ps.Name] = ps.Count
	}
	return counts, nil
}

// SerializePodSetCounts converts PodSets into a JSON byte slice of podSetReplicaSize entries.
func SerializePodSetCounts(podSets []kueue.PodSet) ([]byte, error) {
	sizes := make([]PodSetReplicaSize, len(podSets))
	for i, ps := range podSets {
		sizes[i] = PodSetReplicaSize{Name: ps.Name, Count: ps.Count}
	}
	return json.Marshal(sizes)
}

func GetWorkloadslicingCustomAnnotations(object client.Object, podSets []kueue.PodSet) (map[string]string, error) {
	if workloadslicing.Enabled(object) {
		previousCounts, err := ParsePodSetReplicaSizes(object.GetAnnotations()[PodsetReplicaSizesAnnotation])
		if err != nil {
			return nil, err
		}

		// Compare current counts against previous annotation. If any differ, update the in-memory
		// annotation with ALL current podSet counts (not just the changed ones). The actual API server
		// patch is handled by the reconciler.
		changed := ComparePodSetCounts(podSets, previousCounts)
		if changed {
			podSetsJSON, err := SerializePodSetCounts(podSets)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal updated podsets: %w", err)
			}
			return map[string]string{
				PodsetReplicaSizesAnnotation: string(podSetsJSON),
			}, nil
		}
	}
	return nil, nil
}
