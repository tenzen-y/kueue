/*
Copyright 2021 The Kubernetes Authors.

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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	componentconfigv1alpha1 "k8s.io/component-base/config/v1alpha1"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	cfg "sigs.k8s.io/controller-runtime/pkg/config/v1alpha1"
	ctrlconfigv1alpha1 "sigs.k8s.io/controller-runtime/pkg/config/v1alpha1"

	configv1alpha2 "sigs.k8s.io/kueue/apis/config/v1alpha2"
)

const (
	defaultHealthProbeAddress = ":8081"
	defaultMetricsAddress     = ":8080"
	defaultWebhookPort        = 9443
	defaultLeaderElectionID   = "c1f6bfd2.kueue.x-k8s.io"
	defaultNamespace          = "kueue-system"
	defaultServiceName        = "kueue-webhook-service"
	defaultSecretName         = "kueue-webhook-server-cert"
)

func TestApply(t *testing.T) {
	// temp dir
	tmpDir, err := os.MkdirTemp("", "temp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	namespaceOverWriteConfig := filepath.Join(tmpDir, "namespace-overwrite.yaml")
	if err := os.WriteFile(namespaceOverWriteConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
namespace: kueue-tenant-a
health:
  healthProbeBindAddress: :8081
metrics:
  bindAddress: :8080
leaderElection:
  leaderElect: true
  resourceName: c1f6bfd2.kueue.x-k8s.io
webhook:
  port: 9443
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	ctrlManagerConfigSpecOverWriteConfig := filepath.Join(tmpDir, "ctrl-manager-config-spec-overwrite.yaml")
	if err := os.WriteFile(ctrlManagerConfigSpecOverWriteConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
namespace: kueue-system
health:
  healthProbeBindAddress: :38081
metrics:
  bindAddress: :38080
leaderElection:
  leaderElect: true
  resourceName: test-id
webhook:
  port: 9444
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	certOverWriteConfig := filepath.Join(tmpDir, "cert-overwrite.yaml")
	if err := os.WriteFile(certOverWriteConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
namespace: kueue-system
health:
  healthProbeBindAddress: :8081
metrics:
  bindAddress: :8080
leaderElection:
  leaderElect: true
  resourceName: c1f6bfd2.kueue.x-k8s.io
webhook:
  port: 9443
internalCertManagement:
  enable: true
  serviceName: kueue-tenant-a-webhook-service
  secretName: kueue-tenant-a-webhook-server-cert
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	disableCertOverWriteConfig := filepath.Join(tmpDir, "disable-cert-overwrite.yaml")
	if err := os.WriteFile(disableCertOverWriteConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
namespace: kueue-system
health:
  healthProbeBindAddress: :8081
metrics:
  bindAddress: :8080
leaderElection:
  leaderElect: true
  resourceName: c1f6bfd2.kueue.x-k8s.io
webhook:
  port: 9443
internalCertManagement:
  enable: false
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	defaultControlOptions := ctrl.Options{
		Port:                   9443,
		HealthProbeBindAddress: defaultHealthProbeAddress,
		MetricsBindAddress:     defaultMetricsAddress,
		LeaderElectionID:       defaultLeaderElectionID,
		LeaderElection:         true,
	}

	cmpOpts := []cmp.Option{
		cmpopts.IgnoreFields(ctrl.Options{}, "Scheme",
			"makeBroadcaster",
			"Logger",
			"newRecorderProvider",
			"newResourceLock",
			"newMetricsListener",
			"newHealthProbeListener",
		),
	}

	testcases := []struct {
		name              string
		configFile        string
		wantConfiguration configv1alpha2.Configuration
		wantOptions       ctrl.Options
	}{
		{
			name:       "default config",
			configFile: "",
			wantConfiguration: configv1alpha2.Configuration{
				Namespace: pointer.String(defaultNamespace),
				InternalCertManagement: &configv1alpha2.InternalCertManagement{
					Enable:      pointer.Bool(true),
					ServiceName: pointer.String(defaultServiceName),
					SecretName:  pointer.String(defaultSecretName),
				},
			},
			wantOptions: ctrl.Options{
				Port:                   9443,
				HealthProbeBindAddress: defaultHealthProbeAddress,
				MetricsBindAddress:     defaultMetricsAddress,
				LeaderElectionID:       defaultLeaderElectionID,
				LeaderElection:         false,
			},
		},
		{
			name:       "namespace overwrite config",
			configFile: namespaceOverWriteConfig,
			wantConfiguration: configv1alpha2.Configuration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: configv1alpha2.GroupVersion.String(),
					Kind:       "Configuration",
				},
				Namespace:                  pointer.String("kueue-tenant-a"),
				ManageJobsWithoutQueueName: false,
				ControllerManagerConfigurationSpec: cfg.ControllerManagerConfigurationSpec{
					Health: ctrlconfigv1alpha1.ControllerHealth{
						HealthProbeBindAddress: defaultHealthProbeAddress,
					},
					Metrics: ctrlconfigv1alpha1.ControllerMetrics{
						BindAddress: defaultMetricsAddress,
					},
					LeaderElection: &componentconfigv1alpha1.LeaderElectionConfiguration{
						LeaderElect:  pointer.Bool(true),
						ResourceName: defaultLeaderElectionID,
					},
					Webhook: ctrlconfigv1alpha1.ControllerWebhook{
						Port: pointer.Int(defaultWebhookPort),
					},
				},
				InternalCertManagement: &configv1alpha2.InternalCertManagement{
					Enable:      pointer.Bool(true),
					ServiceName: pointer.String(defaultServiceName),
					SecretName:  pointer.String(defaultSecretName),
				},
			},
			wantOptions: defaultControlOptions,
		},
		{
			name:       "ControllerManagerConfigurationSpec overwrite config",
			configFile: ctrlManagerConfigSpecOverWriteConfig,
			wantConfiguration: configv1alpha2.Configuration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: configv1alpha2.GroupVersion.String(),
					Kind:       "Configuration",
				},
				Namespace:                  pointer.String(defaultNamespace),
				ManageJobsWithoutQueueName: false,
				ControllerManagerConfigurationSpec: cfg.ControllerManagerConfigurationSpec{
					Health: ctrlconfigv1alpha1.ControllerHealth{
						HealthProbeBindAddress: ":38081",
					},
					Metrics: ctrlconfigv1alpha1.ControllerMetrics{
						BindAddress: ":38080",
					},
					LeaderElection: &componentconfigv1alpha1.LeaderElectionConfiguration{
						LeaderElect:  pointer.Bool(true),
						ResourceName: "test-id",
					},
					Webhook: ctrlconfigv1alpha1.ControllerWebhook{
						Port: pointer.Int(9444),
					},
				},
				InternalCertManagement: &configv1alpha2.InternalCertManagement{
					Enable:      pointer.Bool(true),
					ServiceName: pointer.String(defaultServiceName),
					SecretName:  pointer.String(defaultSecretName),
				},
			},
			wantOptions: ctrl.Options{
				HealthProbeBindAddress: ":38081",
				MetricsBindAddress:     ":38080",
				Port:                   9444,
				LeaderElection:         true,
				LeaderElectionID:       "test-id",
			},
		},
		{
			name:       "cert options overwrite config",
			configFile: certOverWriteConfig,
			wantConfiguration: configv1alpha2.Configuration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: configv1alpha2.GroupVersion.String(),
					Kind:       "Configuration",
				},
				Namespace:                  pointer.String(defaultNamespace),
				ManageJobsWithoutQueueName: false,
				ControllerManagerConfigurationSpec: cfg.ControllerManagerConfigurationSpec{
					Health: ctrlconfigv1alpha1.ControllerHealth{
						HealthProbeBindAddress: defaultHealthProbeAddress,
					},
					Metrics: ctrlconfigv1alpha1.ControllerMetrics{
						BindAddress: defaultMetricsAddress,
					},
					LeaderElection: &componentconfigv1alpha1.LeaderElectionConfiguration{
						LeaderElect:  pointer.Bool(true),
						ResourceName: defaultLeaderElectionID,
					},
					Webhook: ctrlconfigv1alpha1.ControllerWebhook{
						Port: pointer.Int(defaultWebhookPort),
					},
				},
				InternalCertManagement: &configv1alpha2.InternalCertManagement{
					Enable:      pointer.Bool(true),
					ServiceName: pointer.String("kueue-tenant-a-webhook-service"),
					SecretName:  pointer.String("kueue-tenant-a-webhook-server-cert"),
				},
			},
			wantOptions: defaultControlOptions,
		},
		{
			name:       "disable cert overwrite config",
			configFile: disableCertOverWriteConfig,
			wantConfiguration: configv1alpha2.Configuration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: configv1alpha2.GroupVersion.String(),
					Kind:       "Configuration",
				},
				Namespace:                  pointer.String(defaultNamespace),
				ManageJobsWithoutQueueName: false,
				ControllerManagerConfigurationSpec: cfg.ControllerManagerConfigurationSpec{
					Health: ctrlconfigv1alpha1.ControllerHealth{
						HealthProbeBindAddress: defaultHealthProbeAddress,
					},
					Metrics: ctrlconfigv1alpha1.ControllerMetrics{
						BindAddress: defaultMetricsAddress,
					},
					LeaderElection: &componentconfigv1alpha1.LeaderElectionConfiguration{
						LeaderElect:  pointer.Bool(true),
						ResourceName: defaultLeaderElectionID,
					},
					Webhook: ctrlconfigv1alpha1.ControllerWebhook{
						Port: pointer.Int(defaultWebhookPort),
					},
				},
				InternalCertManagement: &configv1alpha2.InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
			wantOptions: defaultControlOptions,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			options, config := apply(tc.configFile)
			if diff := cmp.Diff(tc.wantConfiguration, config); diff != "" {
				t.Errorf("Unexpected config (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantOptions, options, cmpOpts...); diff != "" {
				t.Errorf("Unexpected options (-want +got):\n%s", diff)
			}
		})
	}
}
