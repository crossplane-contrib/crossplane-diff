package diffprocessor

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
	gcmp "github.com/google/go-cmp/cmp"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// recordingLogger captures what reaches the wrapped logger, so tests can assert that Debug passes
// through and Info does not (the latter is the whole point of WarningLogger: forwarding Info would
// double-print it under --verbose).
type recordingLogger struct {
	infos  []string
	debugs []string
	values []any
}

func (l *recordingLogger) Info(msg string, _ ...any)  { l.infos = append(l.infos, msg) }
func (l *recordingLogger) Debug(msg string, _ ...any) { l.debugs = append(l.debugs, msg) }

func (l *recordingLogger) WithValues(keysAndValues ...any) logging.Logger {
	return &recordingLogger{values: append(append([]any{}, l.values...), keysAndValues...)}
}

// TestWarningLogger_Info covers the dual emission: an Info call becomes a stderr line for humans AND
// a collected OutputWarning for structured output, and is NOT forwarded to the wrapped logger.
func TestWarningLogger_Info(t *testing.T) {
	tests := map[string]struct {
		msg          string
		kv           []any
		wantStderr   string
		wantWarnings []dt.OutputWarning
	}{
		"NoContext": {
			msg:          "something worth knowing",
			wantStderr:   "WARNING: something worth knowing\n",
			wantWarnings: []dt.OutputWarning{{Message: "something worth knowing"}},
		},
		// Context pairs render in sorted key order, not call order, so repeated runs produce
		// byte-identical stderr (Go map iteration is randomized).
		"ContextSortedInStderr": {
			msg:        "credentials missing",
			kv:         []any{"resource", "XR/one", "attempted", 2, "fetched", 1},
			wantStderr: "WARNING: credentials missing (attempted=2, fetched=1, resource=XR/one)\n",
			wantWarnings: []dt.OutputWarning{{
				Message: "credentials missing",
				Context: map[string]string{"resource": "XR/one", "attempted": "2", "fetched": "1"},
			}},
		},
		// A caller that passes an odd number of arguments has a bug, but dropping the dangling key
		// would hide it. Record it with an empty value so it shows up in the output instead.
		"OddKeyValueCount": {
			msg:        "dangling key",
			kv:         []any{"a", 1, "b"},
			wantStderr: "WARNING: dangling key (a=1, b=)\n",
			wantWarnings: []dt.OutputWarning{{
				Message: "dangling key",
				Context: map[string]string{"a": "1", "b": ""},
			}},
		},
		"NonStringKey": {
			msg:        "odd key type",
			kv:         []any{42, "answer"},
			wantStderr: "WARNING: odd key type (42=answer)\n",
			wantWarnings: []dt.OutputWarning{{
				Message: "odd key type",
				Context: map[string]string{"42": "answer"},
			}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer

			wrapped := &recordingLogger{}
			wl := NewWarningLogger(wrapped, &stderr)

			wl.Info(tt.msg, tt.kv...)

			if diff := gcmp.Diff(tt.wantStderr, stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}

			if diff := gcmp.Diff(tt.wantWarnings, wl.Warnings()); diff != "" {
				t.Errorf("warnings mismatch (-want +got):\n%s", diff)
			}

			if len(wrapped.infos) != 0 {
				t.Errorf("Info must not be forwarded to the wrapped logger (would double-print under --verbose), got %v", wrapped.infos)
			}
		})
	}
}

// TestWarningLogger_DebugPassesThrough confirms tracing is untouched: Debug reaches the wrapped
// logger and is never collected as a warning.
func TestWarningLogger_DebugPassesThrough(t *testing.T) {
	var stderr bytes.Buffer

	wrapped := &recordingLogger{}
	wl := NewWarningLogger(wrapped, &stderr)

	wl.Debug("tracing detail", "resource", "XR/one")

	if diff := gcmp.Diff([]string{"tracing detail"}, wrapped.debugs); diff != "" {
		t.Errorf("wrapped Debug calls mismatch (-want +got):\n%s", diff)
	}

	if got := wl.Warnings(); got != nil {
		t.Errorf("Debug must not be collected as a warning, got %v", got)
	}

	if stderr.Len() != 0 {
		t.Errorf("Debug must not write to stderr, got %q", stderr.String())
	}
}

// TestWarningLogger_WithValues covers the derived-logger contract: accumulated pairs are merged into
// each warning, and derived loggers share ONE collection point rather than each accumulating a
// separate list (otherwise a warning raised through a derived logger would never reach output).
func TestWarningLogger_WithValues(t *testing.T) {
	var stderr bytes.Buffer

	wl := NewWarningLogger(&recordingLogger{}, &stderr)

	derived := wl.WithValues("composition", "xbuckets.example.org")
	derived.Info("first", "attempt", 1)

	nested := derived.WithValues("namespace", "default")
	nested.Info("second")

	want := []dt.OutputWarning{
		{
			Message: "first",
			Context: map[string]string{"composition": "xbuckets.example.org", "attempt": "1"},
		},
		{
			Message: "second",
			Context: map[string]string{"composition": "xbuckets.example.org", "namespace": "default"},
		},
	}

	// Drained from the ROOT logger: warnings raised through derived loggers must land in the same sink,
	// in emission order.
	if diff := gcmp.Diff(want, wl.Warnings()); diff != "" {
		t.Errorf("warnings from root logger mismatch (-want +got):\n%s", diff)
	}
}

// TestWarningLogger_WarningsIsACopy guards the documented copy semantics: a caller that retained an
// earlier result must not see it grow, or the renderer could serialize a slice being mutated.
func TestWarningLogger_WarningsIsACopy(t *testing.T) {
	wl := NewWarningLogger(&recordingLogger{}, &bytes.Buffer{})

	wl.Info("first")

	snapshot := wl.Warnings()

	wl.Info("second")

	if len(snapshot) != 1 {
		t.Errorf("retained snapshot should still have 1 warning, got %d", len(snapshot))
	}

	if len(wl.Warnings()) != 2 {
		t.Errorf("current warnings should have 2 entries, got %d", len(wl.Warnings()))
	}
}

// TestWarningLogger_ConcurrentInfo exercises the sink's mutex. Warnings are raised from clients and
// processors that a future change could parallelize, so the collector must not corrupt under
// concurrent use. Run with -race for this to be meaningful.
func TestWarningLogger_ConcurrentInfo(t *testing.T) {
	var stderr bytes.Buffer

	wl := NewWarningLogger(&recordingLogger{}, &stderr)

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()

			wl.WithValues("worker", i).Info("concurrent warning")
		}(i)
	}

	wg.Wait()

	if got := len(wl.Warnings()); got != goroutines {
		t.Errorf("expected %d warnings, got %d", goroutines, got)
	}

	if got := strings.Count(stderr.String(), "WARNING: concurrent warning"); got != goroutines {
		t.Errorf("expected %d stderr lines, got %d", goroutines, got)
	}
}

// TestWarningLogger_NilStderr confirms the collector still works with no human sink, which is how a
// caller that only wants structured output can use it.
func TestWarningLogger_NilStderr(t *testing.T) {
	wl := NewWarningLogger(tu.TestLogger(t, false), nil)

	wl.Info("no stderr configured")

	if diff := gcmp.Diff([]dt.OutputWarning{{Message: "no stderr configured"}}, wl.Warnings()); diff != "" {
		t.Errorf("warnings mismatch (-want +got):\n%s", diff)
	}
}
