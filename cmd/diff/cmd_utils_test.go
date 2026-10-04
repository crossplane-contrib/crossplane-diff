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

func TestNewRunContext(t *testing.T) {
	type want struct {
		err         error
		interrupted bool
	}

	tests := map[string]struct {
		timeout time.Duration
		// act cancels whatever the case is about: the parent stands in for the signal context.
		act  func(cancelParent, cancelRun context.CancelFunc)
		want want
	}{
		"SignalCancelsAsAnInterruption": {
			timeout: time.Hour,
			act:     func(cancelParent, _ context.CancelFunc) { cancelParent() },
			want:    want{err: context.Canceled, interrupted: true},
		},
		"TimeoutIsNotAnInterruption": {
			timeout: time.Millisecond,
			act:     func(_, _ context.CancelFunc) {},
			want:    want{err: context.DeadlineExceeded},
		},
		"OwnCancelIsNotAnInterruption": {
			timeout: time.Hour,
			act:     func(_, cancelRun context.CancelFunc) { cancelRun() },
			want:    want{err: context.Canceled},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()

			ctx, cancel := newRunContext(parent, tt.timeout)
			defer cancel()

			tt.act(cancelParent, cancel)
			<-ctx.Done()

			if !errors.Is(ctx.Err(), tt.want.err) {
				t.Errorf("ctx.Err() = %v, want %v", ctx.Err(), tt.want.err)
			}

			if got := dp.Interrupted(ctx); got != tt.want.interrupted {
				t.Errorf("Interrupted() = %v, want %v", got, tt.want.interrupted)
			}
		})
	}
}

func TestInterruptedRunResult(t *testing.T) {
	interrupted, cancelInterrupted := context.WithCancelCause(context.Background())
	cancelInterrupted(dp.ErrInterrupted)

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
		"NotInterruptedLeavesResultAlone": {
			ctx:      context.Background(),
			err:      errors.New("boom"),
			code:     dp.ExitCodeToolError,
			wantErr:  "boom",
			wantCode: dp.ExitCodeToolError,
		},
		"NotInterruptedSuccess": {
			ctx:      context.Background(),
			code:     dp.ExitCodeSuccess,
			wantCode: dp.ExitCodeSuccess,
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
