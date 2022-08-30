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
				Namespace: "",
				InternalCertManagement: &InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
			want: &Configuration{
				Namespace: "kueue-system",
				InternalCertManagement: &InternalCertManagement{
					Enable: pointer.Bool(false),
				},
			},
		},
		"defaulting InternalCertManagement": {
			original: &Configuration{
				Namespace: "kueue-tenant-a",
			},
			want: &Configuration{
				Namespace: "kueue-tenant-a",
				InternalCertManagement: &InternalCertManagement{
					Enable:      pointer.Bool(true),
					ServiceName: "kueue-webhook-service",
					SecretName:  "kueue-webhook-server-cert",
				},
			},
		},
		"should not defaulting InternalCertManagement": {
			original: &Configuration{
				Namespace: "kueue-tenant-a",
				InternalCertManagement: &InternalCertManagement{
					Enable:      pointer.Bool(false),
					ServiceName: "",
					SecretName:  "",
				},
			},
			want: &Configuration{
				Namespace: "kueue-tenant-a",
				InternalCertManagement: &InternalCertManagement{
					Enable:      pointer.Bool(false),
					ServiceName: "",
					SecretName:  "",
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
