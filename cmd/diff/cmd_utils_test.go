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
	"strings"
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
		// Forced is the signal handed to the forced-cleanup path, if it ran.
		Forced os.Signal
		// StoppedBeforeForce reports whether the handler was unregistered before the forced path
		// started, which is what makes a third signal Go's default hard kill.
		StoppedBeforeForce bool
		Stops              int
	}

	tests := map[string]struct {
		signals []os.Signal
		// forceHangs makes the forced path never return, like a wedged Docker daemon.
		forceHangs bool
		timeout    time.Duration
		cancel     bool
		want       want
	}{
		"OneSignalCancelsGracefullyAndKeepsTheHandler": {
			// The handler stays registered so that a second signal reaches the forced path rather
			// than killing the process and leaking the containers.
			signals: []os.Signal{syscall.SIGINT},
			timeout: time.Hour,
			want: want{
				Registered:  []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:         context.Canceled.Error(),
				Interrupted: &dp.InterruptedError{Signal: syscall.SIGINT},
			},
		},
		"SecondSignalForcesCleanup": {
			signals: []os.Signal{syscall.SIGINT, syscall.SIGINT},
			timeout: time.Hour,
			want: want{
				Registered:         []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:                context.Canceled.Error(),
				Interrupted:        &dp.InterruptedError{Signal: syscall.SIGINT},
				Forced:             syscall.SIGINT,
				StoppedBeforeForce: true,
				Stops:              1,
			},
		},
		"SecondSIGTERMForcesCleanupWithItsSignal": {
			signals: []os.Signal{syscall.SIGINT, syscall.SIGTERM},
			timeout: time.Hour,
			want: want{
				Registered:         []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:                context.Canceled.Error(),
				Interrupted:        &dp.InterruptedError{Signal: syscall.SIGINT},
				Forced:             syscall.SIGTERM,
				StoppedBeforeForce: true,
				Stops:              1,
			},
		},
		"ThirdSignalIsTheDefaultKillEvenIfForcedCleanupHangs": {
			// The handler is unregistered before the forced path starts, so even while it hangs a
			// third signal is no longer caught: Go's default handling kills the process.
			signals:    []os.Signal{syscall.SIGINT, syscall.SIGINT},
			forceHangs: true,
			timeout:    time.Hour,
			want: want{
				Registered:         []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:                context.Canceled.Error(),
				Interrupted:        &dp.InterruptedError{Signal: syscall.SIGINT},
				Forced:             syscall.SIGINT,
				StoppedBeforeForce: true,
				Stops:              1,
			},
		},
		"TimeoutIsNotAnInterruption": {
			timeout: time.Millisecond,
			want: want{
				Registered: []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:        context.DeadlineExceeded.Error(),
			},
		},
		"CancelUnregistersTheHandler": {
			timeout: time.Hour,
			cancel:  true,
			want: want{
				Registered: []os.Signal{os.Interrupt, syscall.SIGTERM},
				Err:        context.Canceled.Error(),
				Stops:      1,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFakeSignals()

			var (
				forced             os.Signal
				stoppedBeforeForce bool
			)

			forceStarted := make(chan struct{})
			release := make(chan struct{})
			defer close(release)

			onForce := func(sig os.Signal) {
				forced = sig
				stoppedBeforeForce = f.stopCount() > 0

				close(forceStarted)

				if tt.forceHangs {
					<-release
				}
			}

			ctx, cancel := newRunContext(tt.timeout, f.source(), onForce)

			for i, sig := range tt.signals {
				f.ch <- sig

				if i == 0 {
					<-ctx.Done()
				}
			}

			if len(tt.signals) > 1 {
				<-forceStarted
			}

			if tt.cancel {
				cancel()
			}

			<-ctx.Done()

			got := want{
				Registered:         f.registered,
				Err:                ctx.Err().Error(),
				Interrupted:        dp.InterruptCause(ctx),
				Forced:             forced,
				StoppedBeforeForce: stoppedBeforeForce,
				Stops:              f.stopCount(),
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("newRunContext() -want +got:\n%s", diff)
			}

			if !tt.forceHangs {
				cancel()
			}
		})
	}
}

func TestForceCleanup(t *testing.T) {
	removeOK := func(context.Context, string) error { return nil }
	hang := func(ctx context.Context, _ string) error {
		<-ctx.Done()
		// Ignore cancellation, like a remover stuck on a wedged daemon socket.
		select {}
	}
	failFor := func(bad string) dp.ContainerRemover {
		return func(_ context.Context, name string) error {
			if name == bad {
				return errors.New("daemon said no")
			}

			return nil
		}
	}

	tests := map[string]struct {
		sig      os.Signal
		names    []string
		remove   dp.ContainerRemover
		wantCode int
		wantOut  string
	}{
		"NothingToRemove": {
			sig:      syscall.SIGINT,
			remove:   removeOK,
			wantCode: 130,
			wantOut:  "Interrupted again: skipping graceful cleanup.\n",
		},
		"AllRemoved": {
			sig:      syscall.SIGTERM,
			names:    []string{"fn-a", "fn-b"},
			remove:   removeOK,
			wantCode: 143,
			wantOut: "Interrupted again: skipping graceful cleanup.\n" +
				"Forcibly removing 2 function container(s)...\n",
		},
		"SomeUnconfirmed": {
			sig:      syscall.SIGINT,
			names:    []string{"fn-a", "fn-b", "fn-c"},
			remove:   failFor("fn-b"),
			wantCode: 130,
			wantOut: "Interrupted again: skipping graceful cleanup.\n" +
				"Forcibly removing 3 function container(s)...\n" +
				"Could not confirm removal of 1 function container(s). Remove them with:\n" +
				"  docker rm -f fn-b\n",
		},
		"HungRemoverIsCapped": {
			sig:      syscall.SIGINT,
			names:    []string{"fn-a", "fn-b"},
			remove:   hang,
			wantCode: 130,
			wantOut: "Interrupted again: skipping graceful cleanup.\n" +
				"Forcibly removing 2 function container(s)...\n" +
				"Could not confirm removal of 2 function container(s). Remove them with:\n" +
				"  docker rm -f fn-a fn-b\n",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder

			start := time.Now()
			code := forceCleanup(tt.sig, tt.names, tt.remove, &out)

			if elapsed := time.Since(start); elapsed > dp.ForcedCleanupTimeout+time.Second {
				t.Errorf("forceCleanup took %s, want at most about %s", elapsed, dp.ForcedCleanupTimeout)
			}

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}

			if diff := cmp.Diff(tt.wantOut, out.String()); diff != "" {
				t.Errorf("stderr -want +got:\n%s", diff)
			}
		})
	}
}

func TestNewRunContextCancelIsIdempotent(t *testing.T) {
	f := newFakeSignals()

	_, cancel := newRunContext(time.Hour, f.source(), func(os.Signal) {})
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
