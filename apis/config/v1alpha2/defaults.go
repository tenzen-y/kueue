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
	runtime "k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/kueue/pkg/util/pointer"
)

func addDefaultingFuncs(scheme *runtime.Scheme) error {
	scheme.AddTypeDefaultingFunc(&Configuration{}, func(obj interface{}) {
		SetDefaults_Configuration(obj.(*Configuration))
	})
	return nil
}

// SetDefaults_Configuration sets default values for ComponentConfig.
func SetDefaults_Configuration(cfg *Configuration) {
	const (
		defaultNamespace   = "kueue-system"
		defaultServiceName = "kueue-webhook-service"
		defaultSecretName  = "kueue-webhook-server-cert"
	)

	if cfg.Namespace == nil {
		cfg.Namespace = pointer.String(defaultNamespace)
	}
	if cfg.InternalCertManagement == nil {
		cfg.InternalCertManagement = &InternalCertManagement{}
	}
	if cfg.InternalCertManagement.Enable == nil {
		cfg.InternalCertManagement.Enable = pointer.Bool(true)
	}
	if *cfg.InternalCertManagement.Enable {
		if cfg.InternalCertManagement.ServiceName == nil {
			cfg.InternalCertManagement.ServiceName = pointer.String(defaultServiceName)
		}
		if cfg.InternalCertManagement.SecretName == nil {
			cfg.InternalCertManagement.SecretName = pointer.String(defaultSecretName)
		}
	}
}
