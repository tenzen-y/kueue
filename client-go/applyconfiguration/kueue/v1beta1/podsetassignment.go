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
// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1beta1

import (
	v1 "k8s.io/api/core/v1"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

// PodSetAssignmentApplyConfiguration represents a declarative configuration of the PodSetAssignment type for use
// with apply.
type PodSetAssignmentApplyConfiguration struct {
	Name                   *kueuev1beta1.PodSetReference                            `json:"name,omitempty"`
	Flavors                map[v1.ResourceName]kueuev1beta1.ResourceFlavorReference `json:"flavors,omitempty"`
	ResourceUsage          *v1.ResourceList                                         `json:"resourceUsage,omitempty"`
	Count                  *int32                                                   `json:"count,omitempty"`
	TopologyAssignment     *TopologyAssignmentApplyConfiguration                    `json:"topologyAssignment,omitempty"`
	DelayedTopologyRequest *kueuev1beta1.DelayedTopologyRequestState                `json:"delayedTopologyRequest,omitempty"`
}

// PodSetAssignmentApplyConfiguration constructs a declarative configuration of the PodSetAssignment type for use with
// apply.
func PodSetAssignment() *PodSetAssignmentApplyConfiguration {
	return &PodSetAssignmentApplyConfiguration{}
}

// WithName sets the Name field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Name field is set to the value of the last call.
func (b *PodSetAssignmentApplyConfiguration) WithName(value kueuev1beta1.PodSetReference) *PodSetAssignmentApplyConfiguration {
	b.Name = &value
	return b
}

// WithFlavors puts the entries into the Flavors field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, the entries provided by each call will be put on the Flavors field,
// overwriting an existing map entries in Flavors field with the same key.
func (b *PodSetAssignmentApplyConfiguration) WithFlavors(entries map[v1.ResourceName]kueuev1beta1.ResourceFlavorReference) *PodSetAssignmentApplyConfiguration {
	if b.Flavors == nil && len(entries) > 0 {
		b.Flavors = make(map[v1.ResourceName]kueuev1beta1.ResourceFlavorReference, len(entries))
	}
	for k, v := range entries {
		b.Flavors[k] = v
	}
	return b
}

// WithResourceUsage sets the ResourceUsage field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the ResourceUsage field is set to the value of the last call.
func (b *PodSetAssignmentApplyConfiguration) WithResourceUsage(value v1.ResourceList) *PodSetAssignmentApplyConfiguration {
	b.ResourceUsage = &value
	return b
}

// WithCount sets the Count field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Count field is set to the value of the last call.
func (b *PodSetAssignmentApplyConfiguration) WithCount(value int32) *PodSetAssignmentApplyConfiguration {
	b.Count = &value
	return b
}

// WithTopologyAssignment sets the TopologyAssignment field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the TopologyAssignment field is set to the value of the last call.
func (b *PodSetAssignmentApplyConfiguration) WithTopologyAssignment(value *TopologyAssignmentApplyConfiguration) *PodSetAssignmentApplyConfiguration {
	b.TopologyAssignment = value
	return b
}

// WithDelayedTopologyRequest sets the DelayedTopologyRequest field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the DelayedTopologyRequest field is set to the value of the last call.
func (b *PodSetAssignmentApplyConfiguration) WithDelayedTopologyRequest(value kueuev1beta1.DelayedTopologyRequestState) *PodSetAssignmentApplyConfiguration {
	b.DelayedTopologyRequest = &value
	return b
}
