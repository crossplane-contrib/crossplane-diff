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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// parseWithWarningLogger builds a parser over the real cli grammar the way main() does — the
// *dp.WarningLogger and its logging.Logger view bound to the same instance — parses args, and
// returns the kong context so a test can see which instance those two bindings resolve to once
// parsing (and therefore every BeforeApply/AfterApply hook) has run. Any "<file>" placeholder is
// replaced with a real temp file so the commands' AfterApply loaders succeed.
//
// It deliberately does not reuse parseArgs from render_backend_flags_test.go: that helper discards
// the context, which is the only thing this test is interested in.
func parseWithWarningLogger(t *testing.T, warnings *dp.WarningLogger, args ...string) (*kong.Context, error) {
	t.Helper()

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

	parser, err := kong.New(&cli{},
		kong.Name("crossplane-diff"),
		kong.BindTo(warnings, (*logging.Logger)(nil)),
		kong.Bind(warnings),
		// AfterApply on the concrete commands needs an *AppContext binding; a zero value is
		// sufficient because processor construction at parse time is in-memory (no cluster
		// connection until Run).
		kong.Bind(&AppContext{}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	return parser.Parse(resolved)
}

// TestVerboseRebindsWarningLogger pins what verboseFlag.BeforeApply does to the advisory channel.
// It replaces the logger bound in main() with a zap-backed one, and re-wraps that in a fresh
// WarningLogger so Info calls still become user-facing warnings. Two properties matter and neither
// was covered: the *dp.WarningLogger and logging.Logger bindings must land on the same instance
// (they are what writes to stderr and what structured output collects from, so a split would report
// one set of warnings and print another), and without --verbose the logger main() bound must survive
// untouched.
func TestVerboseRebindsWarningLogger(t *testing.T) {
	tests := map[string]struct {
		args []string
		// wantRebound is true when parsing is expected to replace the logger bound at construction.
		wantRebound bool
	}{
		"WithoutVerboseKeepsTheLoggerBoundByMain": {
			args: []string{"comp", "<file>"},
		},
		"VerboseRebindsToAFreshWarningLogger": {
			args:        []string{"--verbose", "comp", "<file>"},
			wantRebound: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer

			original := dp.NewWarningLogger(logging.NewNopLogger(), &stderr)

			// Raised before parsing, so it can only ever be in the original's sink. Whether a
			// rebinding happens or not, this pins where a pre-parse warning ends up.
			original.Info("raised before parsing")

			ctx, err := parseWithWarningLogger(t, original, tt.args...)
			if err != nil {
				t.Fatalf("parse(%v) unexpected error: %v", tt.args, err)
			}

			var (
				asLogger   logging.Logger
				asWarnings *dp.WarningLogger
			)

			if _, err := ctx.Call(func(l logging.Logger, w *dp.WarningLogger) {
				asLogger, asWarnings = l, w
			}); err != nil {
				t.Fatalf("resolve bindings after parse(%v): %v", tt.args, err)
			}

			if asWarnings == nil {
				t.Fatal("*dp.WarningLogger binding resolved to nil")
			}

			if asLogger != logging.Logger(asWarnings) {
				t.Errorf("logging.Logger and *dp.WarningLogger bindings resolve to different instances; the logging.Logger is a %T", asLogger)
			}

			if rebound := asWarnings != original; rebound != tt.wantRebound {
				t.Fatalf("logger rebound = %v, want %v", rebound, tt.wantRebound)
			}

			if !tt.wantRebound {
				return
			}

			// The re-wrap brings its own sink, so a warning raised before the rebinding is not
			// carried over. That is the cost of wrapping twice; pin it so a change in either
			// direction is a deliberate one rather than a silent loss.
			if got := asWarnings.Warnings(); len(got) != 0 {
				t.Errorf("rebound logger carries %d warning(s), want 0: %+v", len(got), got)
			}

			if got := original.Warnings(); len(got) != 1 {
				t.Errorf("logger bound by main() holds %d warning(s), want the 1 raised before parsing", len(got))
			}
		})
	}
}
