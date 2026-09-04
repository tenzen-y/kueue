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

package waitforpodsready

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
)

func TestUnschedulableTimeoutEnabled(t *testing.T) {
	testCases := map[string]struct {
		cfg                     *configapi.WaitForPodsReady
		disableWaitForPodsReady bool
		wantEnabled             bool
	}{
		"nil config": {},
		"unschedulableTimeout unset": {
			cfg: &configapi.WaitForPodsReady{Timeout: metav1.Duration{Duration: 5 * time.Minute}},
		},
		"unschedulableTimeout zero": {
			cfg: &configapi.WaitForPodsReady{
				Timeout:              metav1.Duration{Duration: 5 * time.Minute},
				UnschedulableTimeout: &metav1.Duration{},
			},
		},
		"unschedulableTimeout positive": {
			cfg: &configapi.WaitForPodsReady{
				Timeout:              metav1.Duration{Duration: 5 * time.Minute},
				UnschedulableTimeout: &metav1.Duration{Duration: time.Minute},
			},
			wantEnabled: true,
		},
		"unschedulableTimeout positive but WaitForPodsReady disabled by the feature gate": {
			cfg: &configapi.WaitForPodsReady{
				Timeout:              metav1.Duration{Duration: 5 * time.Minute},
				UnschedulableTimeout: &metav1.Duration{Duration: time.Minute},
			},
			disableWaitForPodsReady: true,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.DisableWaitForPodsReady, tc.disableWaitForPodsReady)
			if got := UnschedulableTimeoutEnabled(tc.cfg); got != tc.wantEnabled {
				t.Errorf("UnschedulableTimeoutEnabled() = %t, want %t", got, tc.wantEnabled)
			}
		})
	}
}
