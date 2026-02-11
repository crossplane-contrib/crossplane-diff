/*
Copyright 2020 The Crossplane Authors.

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

package versioninfo

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{
			name:    "EmptyVersion",
			version: "",
		},
		{
			name:    "ValidVersion",
			version: "v1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the package-level version variable
			version = tt.version

			v := New()
			if v == nil {
				t.Fatal("New() returned nil")
			}

			if v.version != tt.version {
				t.Errorf("New() version = %v, want %v", v.version, tt.version)
			}
		})
	}
}

func TestVersioner_GetVersionString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{
			name:    "EmptyVersion",
			version: "",
			want:    "",
		},
		{
			name:    "ValidVersion",
			version: "v1.2.3",
			want:    "v1.2.3",
		},
		{
			name:    "VersionWithGitHash",
			version: "v0.0.0-1234567890-abc123",
			want:    "v0.0.0-1234567890-abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &Versioner{
				version: tt.version,
			}

			if got := v.GetVersionString(); got != tt.want {
				t.Errorf("GetVersionString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVersioner_GetSemVer(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{
			name:    "ValidSemanticVersion",
			version: "v1.2.3",
			wantErr: false,
		},
		{
			name:    "ValidSemanticVersionWithPrerelease",
			version: "v1.2.3-alpha.1",
			wantErr: false,
		},
		{
			name:    "InvalidVersion",
			version: "not-a-version",
			wantErr: true,
		},
		{
			name:    "EmptyVersion",
			version: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &Versioner{
				version: tt.version,
			}

			got, err := v.GetSemVer()
			if (err != nil) != tt.wantErr {
				t.Errorf("GetSemVer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if got == nil {
					t.Error("GetSemVer() returned nil without error")
					return
				}

				// semver library strips the 'v' prefix, so we need to do the same for comparison
				wantVersion := strings.TrimPrefix(tt.version, "v")

				if got.String() != wantVersion {
					t.Errorf("GetSemVer() version = %v, want %v", got.String(), wantVersion)
				}
			}
		})
	}
}

func TestVersioner_InConstraints(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		constraint string
		want       bool
		wantErr    bool
	}{
		{
			name:       "VersionInConstraint",
			version:    "v1.2.3",
			constraint: "^1.0.0",
			want:       true,
			wantErr:    false,
		},
		{
			name:       "VersionNotInConstraint",
			version:    "v2.0.0",
			constraint: "^1.0.0",
			want:       false,
			wantErr:    false,
		},
		{
			name:       "InvalidVersion",
			version:    "not-a-version",
			constraint: "^1.0.0",
			want:       false,
			wantErr:    true,
		},
		{
			name:       "InvalidConstraint",
			version:    "v1.2.3",
			constraint: "not-a-constraint",
			want:       false,
			wantErr:    true,
		},
		{
			name:       "ExactMatch",
			version:    "v1.2.3",
			constraint: "1.2.3",
			want:       true,
			wantErr:    false,
		},
		{
			name:       "RangeConstraint",
			version:    "v1.5.0",
			constraint: ">=1.0.0, <2.0.0",
			want:       true,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &Versioner{
				version: tt.version,
			}

			got, err := v.InConstraints(tt.constraint)
			if (err != nil) != tt.wantErr {
				t.Errorf("InConstraints() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if got != tt.want {
				t.Errorf("InConstraints() = %v, want %v", got, tt.want)
			}
		})
	}
}
