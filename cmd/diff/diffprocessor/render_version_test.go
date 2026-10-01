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

	gcmp "github.com/google/go-cmp/cmp"
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

func TestNormalizeRenderVersion(t *testing.T) {
	tests := map[string]struct {
		version string
		want    string
	}{
		// The leniency ValidateMinRenderVersion advertises (see
		// "NoLeadingVAccepted" above) only works end to end if the accepted
		// bare form is turned into a tag that actually exists upstream.
		"BareSemverGainsV": {
			version: "2.3.4",
			want:    "v2.3.4",
		},
		"AlreadyPrefixedUnchanged": {
			version: "v2.3.4",
			want:    "v2.3.4",
		},
		"BarePrereleasePreservedVerbatim": {
			version: "2.4.0-rc.1",
			want:    "v2.4.0-rc.1",
		},
		"BareBuildMetadataPreservedVerbatim": {
			version: "2.3.4+build.5",
			want:    "v2.3.4+build.5",
		},
		// Non-semver tags are real, pullable tags; rewriting them would break
		// them.
		"FloatingStableUnchanged": {
			version: "stable",
			want:    "stable",
		},
		"BranchTagUnchanged": {
			version: "main",
			want:    "main",
		},
		"EmptyUnchanged": {
			version: "",
			want:    "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gcmp.Diff(tt.want, NormalizeRenderVersion(tt.version)); diff != "" {
				t.Errorf("NormalizeRenderVersion(%q) mismatch (-want +got):\n%s", tt.version, diff)
			}
		})
	}
}

func TestValidateMinRenderImage(t *testing.T) {
	tests := map[string]struct {
		image string
		// errContains, when set, must appear in the returned error. When empty,
		// no error is expected.
		errContains string
	}{
		"TagAtMinimum": {
			image: "example.com/mirror/crossplane:v2.3.4",
		},
		"TagAboveMinimum": {
			image: "example.com/mirror/crossplane:v2.4.1",
		},
		"TagBelowMinimum": {
			image:       "internal-mirror/crossplane:v2.3.3",
			errContains: MinCrossplaneRenderVersion,
		},
		"BareSemverTagBelowMinimum": {
			image:       "internal-mirror/crossplane:2.3.3",
			errContains: MinCrossplaneRenderVersion,
		},
		"FarBelowMinimum": {
			image:       "xpkg.crossplane.io/crossplane/crossplane:v1.20.12",
			errContains: MinCrossplaneRenderVersion,
		},
		// A ":" before a "/" is a registry port, not a tag separator.
		"RegistryPortWithTagBelowMinimum": {
			image:       "localhost:5000/crossplane/crossplane:v2.3.3",
			errContains: MinCrossplaneRenderVersion,
		},
		"RegistryPortWithTagAboveMinimum": {
			image: "localhost:5000/crossplane/crossplane:v2.4.1",
		},
		"RegistryPortNoTag": {
			image: "localhost:5000/crossplane/crossplane",
		},
		// Uncomparable references must be accepted: refusing them would break
		// the private-mirror and digest-pinning cases the flag exists for.
		"NoTag": {
			image: "internal-mirror/crossplane",
		},
		"FloatingStableTag": {
			image: "xpkg.crossplane.io/crossplane/crossplane:stable",
		},
		"LatestTag": {
			image: "internal-mirror/crossplane:latest",
		},
		"BranchTag": {
			image: "internal-mirror/crossplane:main",
		},
		"DigestPinned": {
			image: "internal-mirror/crossplane@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		// The digest is what docker resolves, so an accompanying tag is
		// advisory and must not be treated as authoritative — not even to fail.
		"DigestPinnedWithBelowMinimumTag": {
			image: "internal-mirror/crossplane:v2.3.3@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateMinRenderImage(tt.image)

			if tt.errContains != "" {
				if err == nil {
					t.Fatalf("ValidateMinRenderImage(%q) = nil, want error", tt.image)
				}

				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidateMinRenderImage(%q) error = %q, want it to contain %q", tt.image, err.Error(), tt.errContains)
				}

				return
			}

			if err != nil {
				t.Errorf("ValidateMinRenderImage(%q) = %v, want nil", tt.image, err)
			}
		})
	}
}

func TestUncomparableRenderImageWarning(t *testing.T) {
	tests := map[string]struct {
		image string
		// wantWarning reports whether a warning is expected. Its text is
		// asserted separately below so every row does not have to restate it.
		wantWarning bool
	}{
		"ComparableTagProducesNoWarning": {
			image:       "example.com/mirror/crossplane:v2.4.1",
			wantWarning: false,
		},
		// A below-minimum tag is a hard failure at the CLI boundary, not an
		// advisory: it must not also produce a warning.
		"BelowMinimumTagProducesNoWarning": {
			image:       "example.com/mirror/crossplane:v2.3.3",
			wantWarning: false,
		},
		"NoTagWarns": {
			image:       "internal-mirror/crossplane",
			wantWarning: true,
		},
		"RegistryPortNoTagWarns": {
			image:       "localhost:5000/crossplane/crossplane",
			wantWarning: true,
		},
		"FloatingTagWarns": {
			image:       "xpkg.crossplane.io/crossplane/crossplane:stable",
			wantWarning: true,
		},
		"DigestPinnedWarns": {
			image:       "internal-mirror/crossplane@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			wantWarning: true,
		},
		// No image selected at all means the engine resolves its own default;
		// that is not the user handing us an uncomparable reference.
		"EmptyProducesNoWarning": {
			image:       "",
			wantWarning: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := UncomparableRenderImageWarning(tt.image)

			if tt.wantWarning {
				if got == "" {
					t.Fatalf("UncomparableRenderImageWarning(%q) = %q, want a warning", tt.image, got)
				}

				// A warning a user cannot act on is not worth printing: it must
				// name the reference and the floor it could not be checked against.
				for _, want := range []string{tt.image, MinCrossplaneRenderVersion} {
					if !strings.Contains(got, want) {
						t.Errorf("UncomparableRenderImageWarning(%q) = %q, want it to contain %q", tt.image, got, want)
					}
				}

				return
			}

			if got != "" {
				t.Errorf("UncomparableRenderImageWarning(%q) = %q, want no warning", tt.image, got)
			}
		})
	}
}
