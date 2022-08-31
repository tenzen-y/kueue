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
				Namespace: nil,
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
					Enable:      pointer.Bool(false),
					ServiceName: nil,
					SecretName:  nil,
				},
			},
			want: &Configuration{
				Namespace: pointer.String("kueue-tenant-a"),
				InternalCertManagement: &InternalCertManagement{
					Enable:      pointer.Bool(false),
					ServiceName: nil,
					SecretName:  nil,
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
