/*
Copyright 2026 The Crossplane Authors.

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

package diffprocessor

import (
	"strings"
	"testing"
)

func TestValidateMinRenderVersion(t *testing.T) {
	tests := map[string]struct {
		version string
		wantErr bool
		// errContains, when set, must appear in the returned error message.
		errContains string
	}{
		"ExactMinimum": {
			version: "v2.3.4",
			wantErr: false,
		},
		"AboveMinimumPatch": {
			version: "v2.3.5",
			wantErr: false,
		},
		"AboveMinimumMinor": {
			version: "v2.4.0",
			wantErr: false,
		},
		"AboveMinimumMajor": {
			version: "v3.0.0",
			wantErr: false,
		},
		"NoLeadingVAccepted": {
			version: "2.3.4",
			wantErr: false,
		},
		"BelowMinimumPatch": {
			version:     "v2.3.3",
			wantErr:     true,
			errContains: MinCrossplaneRenderVersion,
		},
		"BelowMinimumMinor": {
			version:     "v2.0.0",
			wantErr:     true,
			errContains: MinCrossplaneRenderVersion,
		},
		"BelowMinimumMajor": {
			version:     "v1.20.0",
			wantErr:     true,
			errContains: MinCrossplaneRenderVersion,
		},
		"UnparseableStable": {
			version:     "stable",
			wantErr:     true,
			errContains: "stable",
		},
		"UnparseableLatest": {
			version:     "latest",
			wantErr:     true,
			errContains: "latest",
		},
		"Empty": {
			version: "",
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateMinRenderVersion(tt.version)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateMinRenderVersion(%q) = nil, want error", tt.version)
				}

				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidateMinRenderVersion(%q) error = %q, want it to contain %q", tt.version, err.Error(), tt.errContains)
				}

				return
			}

			if err != nil {
				t.Errorf("ValidateMinRenderVersion(%q) = %v, want nil", tt.version, err)
			}
		})
	}
}
