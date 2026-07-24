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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// parseArgs builds a kong parser matching main()'s construction closely enough
// to exercise flag parsing, the crossplane-render-backend xor group, and the
// CommonCmdFields.Validate hook (kong runs Validate during Parse, before the
// concrete command's AfterApply). Any "<file>" placeholder in args is replaced
// with a real temp file path so the commands' AfterApply loaders succeed on the
// happy path. It returns the parsed cli and any parse error.
func parseArgs(t *testing.T, args ...string) (*cli, error) {
	t.Helper()

	// Materialize a real input file so AfterApply's resource loader can stat it
	// on successful parses. Validation/xor errors fire before the loader runs,
	// so those cases never reach it.
	tmp := filepath.Join(t.TempDir(), "input.yaml")
	if err := os.WriteFile(tmp, []byte("apiVersion: example.org/v1\nkind: XR\nmetadata:\n  name: x\n"), 0o600); err != nil {
		t.Fatalf("write temp input: %v", err)
	}

	resolved := make([]string, len(args))
	for i, a := range args {
		if a == "<file>" {
			resolved[i] = tmp
		} else {
			resolved[i] = a
		}
	}

	c := &cli{}

	parser, err := kong.New(c,
		kong.Name("crossplane-diff"),
		kong.BindTo(logging.NewNopLogger(), (*logging.Logger)(nil)),
		// AfterApply on the concrete commands needs an *AppContext binding; a
		// zero value is sufficient because processor construction at parse time
		// is in-memory (no cluster connection until Run).
		kong.Bind(&AppContext{}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	_, err = parser.Parse(resolved)

	return c, err
}

func TestRenderBackendFlags(t *testing.T) {
	tests := map[string]struct {
		args        []string
		wantErr     bool
		errContains string
		// check runs additional assertions on a successful parse.
		check func(t *testing.T, c *cli)
	}{
		"VersionFlagAtMinimum": {
			args: []string{"xr", "--crossplane-version", "v2.3.4", "<file>"},
			check: func(t *testing.T, c *cli) {
				t.Helper()

				if got := c.XR.CrossplaneVersion; got != "v2.3.4" {
					t.Errorf("CrossplaneVersion = %q, want v2.3.4", got)
				}
			},
		},
		"VersionFlagAboveMinimum": {
			args: []string{"xr", "--crossplane-version", "v2.4.0", "<file>"},
			check: func(t *testing.T, c *cli) {
				t.Helper()

				if got := c.XR.CrossplaneVersion; got != "v2.4.0" {
					t.Errorf("CrossplaneVersion = %q, want v2.4.0", got)
				}
			},
		},
		"ImageFlag": {
			args: []string{"xr", "--crossplane-image", "example.com/mirror/crossplane:v2.3.4", "<file>"},
			check: func(t *testing.T, c *cli) {
				t.Helper()

				if got := c.XR.CrossplaneImage; got != "example.com/mirror/crossplane:v2.3.4" {
					t.Errorf("CrossplaneImage = %q, want the mirror ref", got)
				}
			},
		},
		"VersionBelowMinimumRejected": {
			args:        []string{"xr", "--crossplane-version", "v2.3.3", "<file>"},
			wantErr:     true,
			errContains: dp.MinCrossplaneRenderVersion,
		},
		"VersionUnparseableRejected": {
			args:        []string{"xr", "--crossplane-version", "stable", "<file>"},
			wantErr:     true,
			errContains: "stable",
		},
		"VersionAndImageMutuallyExclusive": {
			args:        []string{"xr", "--crossplane-version", "v2.4.0", "--crossplane-image", "foo:bar", "<file>"},
			wantErr:     true,
			errContains: "crossplane-image",
		},
		"CompVersionBelowMinimumRejected": {
			args:        []string{"comp", "--crossplane-version", "v2.0.0", "<file>"},
			wantErr:     true,
			errContains: dp.MinCrossplaneRenderVersion,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c, err := parseArgs(t, tt.args...)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parse(%v) = nil error, want error", tt.args)
				}

				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("parse(%v) error = %q, want it to contain %q", tt.args, err.Error(), tt.errContains)
				}

				return
			}

			if err != nil {
				t.Fatalf("parse(%v) unexpected error: %v", tt.args, err)
			}

			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}
