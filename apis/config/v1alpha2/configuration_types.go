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

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cfg "sigs.k8s.io/controller-runtime/pkg/config/v1alpha1"
)

//+kubebuilder:object:root=true

// Configuration is the Schema for the kueueconfigurations API
type Configuration struct {
	metav1.TypeMeta `json:",inline"`

	// Namespace is the namespace in which kueue is deployed and is also used as part of DNSName.
	// Defaults to kueue-system.
	Namespace string `json:"namespace,omitempty"`

	// ControllerManagerConfigurationSpec returns the configurations for controllers
	cfg.ControllerManagerConfigurationSpec `json:",inline"`

	// ManageJobsWithoutQueueName controls whether or not Kueue reconciles
	// batch/v1.Jobs that don't set the annotation kueue.x-k8s.io/queue-name.
	// If set to true, then those jobs will be suspended and never started unless
	// they are assigned a queue and eventually admitted. This also applies to
	// jobs created before starting the kueue controller.
	// Defaults to false; therefore, those jobs are not managed and if they are created
	// unsuspended, they will start immediately.
	ManageJobsWithoutQueueName bool `json:"manageJobsWithoutQueueName"`

	// InternalCertManagement is configuration for internalCertManagement
	InternalCertManagement *InternalCertManagement `json:"internalCertManagement,omitempty"`
}

type InternalCertManagement struct {

	// Enable controls whether to enable internal cert management or not.
	// Defaults to true. If you want to use a third-party management, e.g. cert-manager,
	// set it to false. See the user guide for more information.
	Enable *bool `json:"enable,omitempty"`

	// ServiceName is used as part of the DNSName.
	// Default to kueue-webhook-service.
	ServiceName string `json:"serviceName,omitempty"`

	// SecretName is used to store CA and server certs.
	// Default to kueue-webhook-server-cert.
	SecretName string `json:"secretName,omitempty"`
}
