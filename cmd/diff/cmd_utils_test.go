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
	"context"
	"errors"
	"testing"
	"time"

	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
)

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
