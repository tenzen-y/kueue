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

	"sigs.k8s.io/kueue/apis/config/v1alpha2"
	"sigs.k8s.io/kueue/pkg/util/pointer"
)

func TestApply(t *testing.T) {
	// temp dir
	tmpDir, err := os.MkdirTemp("", "temp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	portOverWriteConfig := filepath.Join(tmpDir, "port-overwrite.yaml")
	if err := os.WriteFile(portOverWriteConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
leaderElection:
  leaderElect: true
webhook:
  port: 9444
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	internalCertManagementDisableConfig := filepath.Join(tmpDir, "internal-cert-management-disable.yaml")
	if err := os.WriteFile(internalCertManagementDisableConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
leaderElection:
  leaderElect: true
internalCertManagement:
  enable: false
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	certOptionsOverWriteConfig := filepath.Join(tmpDir, "cert-options-overwrite.yaml")
	if err := os.WriteFile(certOptionsOverWriteConfig, []byte(`
apiVersion: config.kueue.x-k8s.io/v1alpha2
kind: Configuration
leaderElection:
 leaderElect: true
internalCertManagement:
 enable: true
 namespace: tenant-a
 serviceName: tenant-a-service
 secretName: tenant-a-secret
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	defaultInternalCertManagement := v1alpha2.InternalCertManagement{
		Enable:      pointer.Bool(true),
		Namespace:   "kueue-system",
		ServiceName: "kueue-webhook-service",
		SecretName:  "kueue-webhook-server-cert",
	}

	testcases := []struct {
		name                       string
		configFile                 string
		wantPort                   int
		wantInternalCertManagement v1alpha2.InternalCertManagement
	}{
		{
			name:                       "default config",
			configFile:                 "",
			wantPort:                   9443,
			wantInternalCertManagement: defaultInternalCertManagement,
		},
		{
			name:                       "port overwrite config",
			configFile:                 portOverWriteConfig,
			wantPort:                   9444,
			wantInternalCertManagement: defaultInternalCertManagement,
		},
		{
			name:       "internal cert management disable config",
			configFile: internalCertManagementDisableConfig,
			wantPort:   9443,
			wantInternalCertManagement: v1alpha2.InternalCertManagement{
				Enable: pointer.Bool(false),
			},
		},
		{
			name:       "cert options overwrite config",
			configFile: certOptionsOverWriteConfig,
			wantPort:   9443,
			wantInternalCertManagement: v1alpha2.InternalCertManagement{
				Enable:      pointer.Bool(true),
				Namespace:   "tenant-a",
				ServiceName: "tenant-a-service",
				SecretName:  "tenant-a-secret",
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			options, config := apply(tc.configFile)

			if options.Port != tc.wantPort {
				t.Fatalf("got unexpected port, want: %v, got: %v", tc.wantPort, options.Port)
			}

			if diff := cmp.Diff(tc.wantInternalCertManagement, config.InternalCertManagement); diff != "" {
				t.Fatalf("got unexpected internalCertManagement, want: %v, got: %v", tc.wantInternalCertManagement, config.InternalCertManagement)
			}
		})
	}
}
