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
	"testing"

	"github.com/google/go-cmp/cmp"

	"sigs.k8s.io/kueue/pkg/util/pointer"
)

func TestSetDefaults_Configuration(t *testing.T) {
	testCases := map[string]struct {
		original *Configuration
		want     *Configuration
	}{
		"defaulting namespace": {
			original: &Configuration{
				InternalCertManagement: &InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
			want: &Configuration{
				Namespace: pointer.String("kueue-system"),
				InternalCertManagement: &InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
		},
		"defaulting InternalCertManagement": {
			original: &Configuration{
				Namespace: pointer.String("kueue-tenant-a"),
			},
			want: &Configuration{
				Namespace: pointer.String("kueue-tenant-a"),
				InternalCertManagement: &InternalCertManagement{
					Enable:      pointer.Bool(true),
					ServiceName: pointer.String("kueue-webhook-service"),
					SecretName:  pointer.String("kueue-webhook-server-cert"),
				},
			},
		},
		"should not defaulting InternalCertManagement": {
			original: &Configuration{
				Namespace: pointer.String("kueue-tenant-a"),
				InternalCertManagement: &InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
			want: &Configuration{
				Namespace: pointer.String("kueue-tenant-a"),
				InternalCertManagement: &InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			SetDefaults_Configuration(tc.original)
			if diff := cmp.Diff(tc.want, tc.original); diff != "" {
				t.Errorf("unexpected error (-want,+got):\n%s", diff)
			}
		})
	}
}
