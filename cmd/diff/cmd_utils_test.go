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
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
	"github.com/google/go-cmp/cmp"
)

// fakeSignals records the signal registration that newRunContext makes, and lets a test deliver a
// signal to it without signalling the test process.
type fakeSignals struct {
	mu         sync.Mutex
	ch         chan<- os.Signal
	registered []os.Signal
	stops      int
	stopped    chan struct{}
}

func newFakeSignals() *fakeSignals { return &fakeSignals{stopped: make(chan struct{}, 8)} }

func (f *fakeSignals) source() signalSource {
	return signalSource{
		notify: func(c chan<- os.Signal, sig ...os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()

			f.ch = c
			f.registered = sig
		},
		stop: func(chan<- os.Signal) {
			f.mu.Lock()
			f.stops++
			f.mu.Unlock()

			f.stopped <- struct{}{}
		},
	}
}

func (f *fakeSignals) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.stops
}

func TestNewRunContext(t *testing.T) {
	type want struct {
		Registered  []os.Signal
		Err         string
		Interrupted *dp.InterruptedError
		Stopped     bool
	}

	tests := map[string]struct {
		timeout time.Duration
		// act drives the context; it returns once the context's fate is decided.
		act  func(t *testing.T, f *fakeSignals, ctx context.Context, cancel context.CancelFunc)
		want want
	}{
		"FirstSignalCancelsAndRestoresDefaultHandling": {
			// Stopping the handler after the first signal is what lets a second Ctrl+C kill the
			// process with Go's default behaviour if cleanup hangs.
			timeout: time.Hour,
			act: func(t *testing.T, f *fakeSignals, ctx context.Context, _ context.CancelFunc) {
				t.Helper()
				f.ch <- syscall.SIGINT
				<-ctx.Done()
				<-f.stopped
			},
			want: want{
				Registered:  []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:         context.Canceled.Error(),
				Interrupted: &dp.InterruptedError{Signal: syscall.SIGINT},
				Stopped:     true,
			},
		},
		"SIGTERMCancels": {
			timeout: time.Hour,
			act: func(t *testing.T, f *fakeSignals, ctx context.Context, _ context.CancelFunc) {
				t.Helper()
				f.ch <- syscall.SIGTERM
				<-ctx.Done()
				<-f.stopped
			},
			want: want{
				Registered:  []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:         context.Canceled.Error(),
				Interrupted: &dp.InterruptedError{Signal: syscall.SIGTERM},
				Stopped:     true,
			},
		},
		"TimeoutIsNotAnInterruption": {
			timeout: time.Millisecond,
			act: func(t *testing.T, _ *fakeSignals, ctx context.Context, _ context.CancelFunc) {
				t.Helper()
				<-ctx.Done()
			},
			want: want{
				Registered: []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:        context.DeadlineExceeded.Error(),
			},
		},
		"CancelUnregistersTheHandler": {
			timeout: time.Hour,
			act: func(t *testing.T, f *fakeSignals, _ context.Context, cancel context.CancelFunc) {
				t.Helper()
				cancel()
				<-f.stopped
			},
			want: want{
				Registered: []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:        context.Canceled.Error(),
				Stopped:    true,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFakeSignals()

			ctx, cancel := newRunContext(tt.timeout, f.source())
			defer cancel()

			tt.act(t, f, ctx, cancel)

			got := want{
				Registered:  f.registered,
				Err:         ctx.Err().Error(),
				Interrupted: dp.InterruptCause(ctx),
				Stopped:     f.stopCount() > 0,
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("newRunContext() -want +got:\n%s", diff)
			}
		})
	}
}

func TestNewRunContextCancelIsIdempotent(t *testing.T) {
	f := newFakeSignals()

	_, cancel := newRunContext(time.Hour, f.source())
	cancel()
	cancel()

	if got := f.stopCount(); got != 1 {
		t.Errorf("stop called %d times, want 1", got)
	}
}

func TestInterruptedRunResult(t *testing.T) {
	interrupted, cancelInterrupted := context.WithCancelCause(context.Background())
	cancelInterrupted(&dp.InterruptedError{Signal: syscall.SIGINT})

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
			wantErr:  "run interrupted by signal (interrupt); results are incomplete",
			wantCode: dp.ExitCodeInterrupted,
		},
		"InterruptedEvenWithoutError": {
			ctx:      interrupted,
			code:     dp.ExitCodeDiffDetected,
			wantErr:  "run interrupted by signal (interrupt); results are incomplete",
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
