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
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// parseWithWarningLogger builds a parser over the real cli grammar with the same warning-logger
// wiring main() uses, parses args, and returns the kong context so a test can see which instance the
// *dp.WarningLogger and logging.Logger bindings resolve to once parsing (and therefore every
// BeforeApply/AfterApply hook) has run. Any "<file>" placeholder is replaced with a real temp file so
// the commands' AfterApply loaders succeed.
//
// It deliberately does not reuse parseArgs from render_backend_flags_test.go: that helper discards
// the context, which is the only thing this test is interested in.
func parseWithWarningLogger(t *testing.T, stderr io.Writer, args ...string) (*kong.Context, error) {
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
		// AfterApply on the concrete commands needs an *AppContext binding; a zero value is
		// sufficient because processor construction at parse time is in-memory (no cluster
		// connection until Run).
		kong.Bind(&AppContext{}),
	}, warningLoggerBindings(c, stderr)...)

	parser, err := kong.New(c, opts...)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	return parser.Parse(resolved)
}

// TestWarningLoggerConstructedOnce pins the warning-logger wiring. kong does not memoize providers,
// so every consumer of *dp.WarningLogger or logging.Logger (the commands' AfterApply, Run, and the
// AppContext provider) must nonetheless see one instance: it is both what writes to stderr and what
// structured output collects from, so a split would report one set of warnings and print another,
// and a second construction would drop anything already collected. That must hold with and without
// --verbose, which only changes the logger the WarningLogger wraps.
func TestWarningLoggerConstructedOnce(t *testing.T) {
	tests := map[string]struct {
		args []string
	}{
		"WithoutVerbose": {
			args: []string{"comp", "<file>"},
		},
		"WithVerbose": {
			args: []string{"--verbose", "comp", "<file>"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer

			ctx, err := parseWithWarningLogger(t, &stderr, tt.args...)
			if err != nil {
				t.Fatalf("parse(%v) unexpected error: %v", tt.args, err)
			}

			resolve := func() (logging.Logger, *dp.WarningLogger) {
				var (
					l logging.Logger
					w *dp.WarningLogger
				)

				if _, err := ctx.Call(func(gotL logging.Logger, gotW *dp.WarningLogger) {
					l, w = gotL, gotW
				}); err != nil {
					t.Fatalf("resolve bindings after parse(%v): %v", tt.args, err)
				}

				return l, w
			}

			firstLogger, first := resolve()
			if first == nil {
				t.Fatal("*dp.WarningLogger binding resolved to nil")
			}

			if firstLogger != logging.Logger(first) {
				t.Errorf("logging.Logger and *dp.WarningLogger bindings resolve to different instances; the logging.Logger is a %T", firstLogger)
			}

			first.Info("raised after parsing")

			secondLogger, second := resolve()
			if second != first || secondLogger != logging.Logger(first) {
				t.Fatal("a second resolution produced a different WarningLogger; the provider must construct it once")
			}

			if got := second.Warnings(); len(got) != 1 {
				t.Errorf("WarningLogger holds %d warning(s), want the 1 raised earlier: %+v", len(got), got)
			}

			if !strings.Contains(stderr.String(), "raised after parsing") {
				t.Errorf("warning did not reach the stderr writer; got %q", stderr.String())
			}
		})
	}
}
