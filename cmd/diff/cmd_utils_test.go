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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer"
	"github.com/google/go-cmp/cmp"

	xperrors "github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

func TestDiffOptions(t *testing.T) {
	parser, err := kong.New(&struct{}{})
	if err != nil {
		t.Fatalf("kong.New(): %v", err)
	}

	kongCtx, err := parser.Parse([]string{})
	if err != nil {
		t.Fatalf("Parse(): %v", err)
	}

	kongCtx.Stdout, kongCtx.Stderr = &bytes.Buffer{}, &bytes.Buffer{}

	tests := map[string]struct {
		fields CommonCmdFields
		want   renderer.DiffOptions
	}{
		// Unset flags (as a test or a non-CLI caller builds them) select the human-readable diff and
		// the renderer's default output version.
		"UnsetFlagsSelectHumanDiff": {
			want: func() renderer.DiffOptions {
				o := renderer.DefaultDiffOptions()
				o.UseColors = true
				o.Format = renderer.OutputFormatDiff

				return o
			}(),
		},
		"StructuredFlagsCarryThrough": {
			fields: CommonCmdFields{Output: "json", OutputVersion: "v2", NoColor: true, Compact: true},
			want: func() renderer.DiffOptions {
				o := renderer.DefaultDiffOptions()
				o.UseColors = false
				o.Compact = true
				o.Format = renderer.OutputFormatJSON
				o.OutputVersion = renderer.OutputVersionV2

				return o
			}(),
		},
		"UnknownFormatFallsBackToHumanDiff": {
			fields: CommonCmdFields{Output: "xml"},
			want: func() renderer.DiffOptions {
				o := renderer.DefaultDiffOptions()
				o.UseColors = true
				o.Format = renderer.OutputFormatDiff

				return o
			}(),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tt.want.Stdout = kongCtx.Stdout
			tt.want.Stderr = kongCtx.Stderr

			got := diffOptions(tt.fields, kongCtx)
			// Writers are interfaces holding the kong context's streams; compare them by identity.
			if got.Stdout != tt.want.Stdout || got.Stderr != tt.want.Stderr {
				t.Errorf("diffOptions() writers: got (%v, %v), want the kong context's", got.Stdout, got.Stderr)
			}

			got.Stdout, got.Stderr, tt.want.Stdout, tt.want.Stderr = nil, nil, nil, nil
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("diffOptions() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWithRenderError pins that a render failure reads exactly as it did when the processor joined it
// with the run's own errors: one flat list, not a list nested in a list.
func TestWithRenderError(t *testing.T) {
	a, b, render := errors.New("a"), errors.New("b"), errors.New("failed to render diffs: boom")

	tests := map[string]struct {
		err  error
		want string
	}{
		"NoRunError":     {err: nil, want: xperrors.Join(render).Error()},
		"SingleRunError": {err: a, want: xperrors.Join(a, render).Error()},
		"JoinedRunError": {err: xperrors.Join(a, b), want: xperrors.Join(a, b, render).Error()},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := withRenderError(tt.err, render).Error(); got != tt.want {
				t.Errorf("withRenderError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterruptedRunResult(t *testing.T) {
	// The run context is a timeout context on the signal context, as in initializeAppContext; each case
	// is evaluated before the run's own cancel, as it is in Run.
	parent, cancelParent := context.WithCancel(context.Background())
	interrupted, cancelInterrupted := context.WithTimeout(parent, time.Hour)
	defer cancelInterrupted()
	cancelParent()

	timedOut, cancelTimedOut := context.WithTimeout(context.Background(), 0)
	defer cancelTimedOut()

	tests := map[string]struct {
		ctx      context.Context
		err      error
		code     int
		wantErr  string
		wantCode int
	}{
		"InterruptedOverridesErrorAndCode": {
			ctx:      interrupted,
			err:      errors.New("unable to process one or more resources: context canceled"),
			code:     dp.ExitCodeToolError,
			wantErr:  "run interrupted by signal; results are incomplete",
			wantCode: dp.ExitCodeInterrupted,
		},
		"InterruptedEvenWithoutError": {
			ctx:      interrupted,
			code:     dp.ExitCodeDiffDetected,
			wantErr:  "run interrupted by signal; results are incomplete",
			wantCode: dp.ExitCodeInterrupted,
		},
		"TimeoutIsNotAnInterruption": {
			ctx:      timedOut,
			err:      errors.New("unable to process one or more resources: context deadline exceeded"),
			code:     dp.ExitCodeToolError,
			wantErr:  "unable to process one or more resources: context deadline exceeded",
			wantCode: dp.ExitCodeToolError,
		},
		"CompletedRunLeavesResultAlone": {
			ctx:      context.Background(),
			err:      errors.New("boom"),
			code:     dp.ExitCodeToolError,
			wantErr:  "boom",
			wantCode: dp.ExitCodeToolError,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			exitCode := &ExitCode{Code: tt.code}
			err := interruptedRunResult(tt.ctx, tt.err, exitCode)

			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}

			if gotErr != tt.wantErr {
				t.Errorf("error = %q, want %q", gotErr, tt.wantErr)
			}

			if exitCode.Code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", exitCode.Code, tt.wantCode)
			}
		})
	}
}
