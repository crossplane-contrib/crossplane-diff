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
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	gcmp "github.com/google/go-cmp/cmp"
	zapraw "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// parseWithLoggingBindings builds a parser over the real cli grammar with the same logger and
// advisory wiring main() uses, parses args, and returns the kong context so a test can see which
// instances the logging.Logger and *dp.AdvisoryCollector bindings resolve to once parsing (and
// therefore every BeforeApply/AfterApply hook) has run. Any "<file>" placeholder is replaced with a
// real temp file so the commands' AfterApply loaders succeed.
//
// It deliberately does not reuse parseArgs from render_backend_flags_test.go: that helper discards
// the context, which is the only thing this test is interested in.
func parseWithLoggingBindings(t *testing.T, stderr io.Writer, args ...string) (*kong.Context, error) {
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

	c := &cli{}

	opts := append([]kong.Option{
		kong.Name("crossplane-diff"),
		engineLimitVars(),
		// AfterApply on the concrete commands needs an *AppContext binding; a zero value is
		// sufficient because processor construction at parse time is in-memory (no cluster
		// connection until Run).
		kong.Bind(&AppContext{}),
	}, loggingBindings(stderr)...)

	parser, err := kong.New(c, opts...)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	return parser.Parse(resolved)
}

// TestLoggingBindings pins the logger and advisory wiring. Every consumer of *dp.AdvisoryCollector
// (the commands' AfterApply and Run) must see one instance: it is both what writes to stderr and what
// structured output collects from, so a split would report one set of advisories and print another.
// The logger is for tracing only: an Info on it must never become an advisory, and it is muted
// unless --verbose, which raises it to Debug.
func TestLoggingBindings(t *testing.T) {
	tests := map[string]struct {
		args      []string
		wantDebug bool
	}{
		"WithoutVerbose": {
			args: []string{"comp", "<file>"},
		},
		"WithVerbose": {
			args:      []string{"--verbose", "comp", "<file>"},
			wantDebug: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer

			ctx, err := parseWithLoggingBindings(t, &stderr, tt.args...)
			if err != nil {
				t.Fatalf("parse(%v) unexpected error: %v", tt.args, err)
			}

			resolve := func() (logging.Logger, *dp.AdvisoryCollector) {
				var (
					l logging.Logger
					a *dp.AdvisoryCollector
				)

				if _, err := ctx.Call(func(gotL logging.Logger, gotA *dp.AdvisoryCollector, level zapraw.AtomicLevel) {
					l, a = gotL, gotA
					if got := level.Enabled(zapcore.DebugLevel); got != tt.wantDebug {
						t.Errorf("Debug enabled = %v, want %v", got, tt.wantDebug)
					}
					if level.Enabled(zapcore.FatalLevel) != tt.wantDebug {
						t.Errorf("Fatal enabled = %v, want %v; the logger must be fully muted without --verbose", !tt.wantDebug, tt.wantDebug)
					}
				}); err != nil {
					t.Fatalf("resolve bindings after parse(%v): %v", tt.args, err)
				}

				return l, a
			}

			firstLogger, first := resolve()
			if first == nil {
				t.Fatal("*dp.AdvisoryCollector binding resolved to nil")
			}

			firstLogger.Info("logged, not advised")
			first.Raise(dt.OutputWarning{Message: "raised after parsing"})

			secondLogger, second := resolve()
			if second != first {
				t.Fatal("a second resolution produced a different AdvisoryCollector; bindings must share one instance")
			}

			if secondLogger != firstLogger {
				t.Fatal("a second resolution produced a different logging.Logger; bindings must share one instance")
			}

			want := []dt.OutputWarning{{Message: "raised after parsing"}}
			if diff := gcmp.Diff(want, second.Advisories()); diff != "" {
				t.Errorf("advisories mismatch; a logger Info must not become an advisory (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff("WARNING: raised after parsing\n", stderr.String()); diff != "" {
				t.Errorf("stderr mismatch; a logger Info must not reach the advisory stderr (-want +got):\n%s", diff)
			}
		})
	}
}
