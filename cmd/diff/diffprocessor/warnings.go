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

package diffprocessor

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// WarningLogger is the advisory channel: a logging.Logger decorator that treats every Info call as a
// user-facing warning, writing it to stderr immediately and collecting it for structured output.
// Debug calls are forwarded to the wrapped logger unchanged.
//
// # Why the logger, and why Info
//
// A warning has to be raisable from wherever it is discovered — a client, a resource manager, the
// render loop. The logging.Logger is the only cross-cutting dependency already threaded to all of
// those; every component factory in ProcessorConfig takes exactly one (a client, a converter, and a
// Logger), so a parallel "warning sink" parameter would mean churning every constructor, every
// factory signature, and every With*Factory option for no semantic gain.
//
// There is no Warn to reach for: crossplane-runtime's logging.Logger has exactly two levels by
// design ("Crossplane prefers not to use go-logr because it desires a simpler API with only two
// levels ... Info and Debug"), and it defines Info as "messages that Crossplane operators are very
// likely to be concerned with when running Crossplane" against Debug's "when debugging Crossplane".
// So treating Info as the advisory level is not a convention invented here — it is the interface's
// documented meaning, which this type implements by giving those messages somewhere to go. What it
// does add is the second sink: the same message also lands in structured output.
//
// The user-facing label is deliberately WARNING rather than INFO. An operator reading
// "INFO: applying this diff will assume ownership" would under-weight it; the upstream *level* and
// the word shown to a human are answering different questions.
//
// # Why it writes to stderr itself
//
// Raising the wrapped logger's verbosity would surface warnings for humans, but in zap's
// development-mode format (timestamp, level, caller) and still nowhere in structured output. Owning
// the human rendering here means one mechanism produces both views — the same dual-emission contract
// OutputError follows — and the text stays fit for a CLI. Info is therefore NOT forwarded to the
// wrapped logger; otherwise a --verbose run would print every warning twice, once in each format.
//
// # Why identical warnings are collapsed
//
// A warning is raised where the condition is discovered, and several of those sites sit inside loops
// the caller chose — resolveFunctionCredentials runs once per XR, so a single unfetchable secret on a
// composition affecting thirty XRs would otherwise produce thirty byte-identical WARNING lines and
// thirty identical warnings[] entries. A repeat that is identical in BOTH message and context carries
// no information the user has not already been given, so the sink keeps only the first. Warnings whose
// context differs in any pair are distinct occurrences and every one of them is kept: the
// ownership-theft advisory in resource_manager.go names the composed resource in its context and must
// fire once per resource. The consequence for a warning site is therefore a simple rule — put what
// distinguishes one occurrence from another in the context, which it should be doing regardless, since
// context is what a machine consumer reads.
//
// A zero WarningLogger is not usable; construct one with NewWarningLogger.
type WarningLogger struct {
	// sink is shared by every logger derived via WithValues, so warnings raised through a derived
	// logger are collected once, centrally, in emission order.
	sink *warningSink

	// values are the accumulated WithValues key/value pairs for this logger, prepended to the
	// per-call pairs so context attached by a caller upstream is not lost.
	values []any

	wrapped logging.Logger
}

// warningSink is the shared, concurrency-safe collection point behind a WarningLogger and every
// logger derived from it.
type warningSink struct {
	mu       sync.Mutex
	stderr   io.Writer
	warnings []dt.OutputWarning
	// seen holds the identity of every warning already recorded, so an identical repeat is dropped
	// from both channels rather than re-emitted. Keyed by warningKey.
	seen map[string]struct{}
}

// NewWarningLogger wraps logger so that Info calls become user-facing warnings written to stderr and
// collected for structured output. Debug calls pass through to logger unchanged.
func NewWarningLogger(logger logging.Logger, stderr io.Writer) *WarningLogger {
	return &WarningLogger{
		sink:    &warningSink{stderr: stderr, seen: map[string]struct{}{}},
		values:  nil,
		wrapped: logger,
	}
}

// Info records msg as a user-facing warning: written to stderr now, and collected for structured
// output. A warning identical to one already raised — same message and same context — is dropped from
// both channels; see the type comment. It is deliberately not forwarded to the wrapped logger.
func (l *WarningLogger) Info(msg string, keysAndValues ...any) {
	warning := dt.OutputWarning{
		Message: msg,
		Context: contextFromKeysAndValues(append(append([]any{}, l.values...), keysAndValues...)),
	}

	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()

	key := warningKey(warning)
	if _, repeat := l.sink.seen[key]; repeat {
		return
	}

	l.sink.seen[key] = struct{}{}
	l.sink.warnings = append(l.sink.warnings, warning)

	if l.sink.stderr != nil {
		// A failed write to stderr is not worth failing a diff over, and there is nowhere useful to
		// report it — the reporting channel is the thing that just failed.
		_, _ = fmt.Fprintln(l.sink.stderr, warning.FormatWarning())
	}
}

// Debug forwards to the wrapped logger. Tracing is not advisory output.
func (l *WarningLogger) Debug(msg string, keysAndValues ...any) {
	l.wrapped.Debug(msg, append(append([]any{}, l.values...), keysAndValues...)...)
}

// WithValues returns a logger that carries the supplied pairs, sharing this logger's collection
// point so derived loggers do not each accumulate their own separate warning list.
func (l *WarningLogger) WithValues(keysAndValues ...any) logging.Logger {
	return &WarningLogger{
		sink:    l.sink,
		values:  append(append([]any{}, l.values...), keysAndValues...),
		wrapped: l.wrapped.WithValues(keysAndValues...),
	}
}

// Warnings returns the warnings collected so far, in emission order. The returned slice is a copy,
// so callers may retain it while more warnings are raised.
func (l *WarningLogger) Warnings() []dt.OutputWarning {
	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()

	if len(l.sink.warnings) == 0 {
		return nil
	}

	out := make([]dt.OutputWarning, len(l.sink.warnings))
	copy(out, l.sink.warnings)

	return out
}

// warningKey builds the dedup identity of a warning: its message followed by its context pairs in
// sorted key order, so a map's randomized iteration and a caller's argument order cannot produce two
// keys for one warning. Every component is rendered with %q, which escapes the quote and backslash
// characters, so a key or value that happens to contain the separators cannot be mistaken for a
// differently split pair — {"a": "b=c"} and {"a=b": "c"} are different warnings and get different keys.
func warningKey(w dt.OutputWarning) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%q", w.Message)

	for _, k := range slices.Sorted(maps.Keys(w.Context)) {
		fmt.Fprintf(&b, " %q=%q", k, w.Context[k])
	}

	return b.String()
}

// contextFromKeysAndValues converts a logr-style variadic key/value list into the string map carried
// on OutputWarning. Keys are rendered with %v so a non-string key still produces a usable entry
// rather than being dropped; a trailing key with no value is recorded with an empty value rather
// than discarded, so a malformed call site is visible in the output instead of silently truncated.
func contextFromKeysAndValues(keysAndValues []any) map[string]string {
	if len(keysAndValues) == 0 {
		return nil
	}

	ctx := make(map[string]string, (len(keysAndValues)+1)/2)

	for i := 0; i < len(keysAndValues); i += 2 {
		key := fmt.Sprintf("%v", keysAndValues[i])

		value := ""
		if i+1 < len(keysAndValues) {
			value = fmt.Sprintf("%v", keysAndValues[i+1])
		}

		ctx[key] = value
	}

	return ctx
}

// collectedWarnings returns the warnings raised so far for inclusion in structured output, or nil
// when no collector was wired. Defined on DefaultDiffProcessor and DefaultCompDiffProcessor rather
// than inlined at each call site so the nil-collector case is handled in one place.
func (p *DefaultDiffProcessor) collectedWarnings() []dt.OutputWarning {
	if p.config.Warnings == nil {
		return nil
	}

	return p.config.Warnings.Warnings()
}

// collectedWarnings mirrors DefaultDiffProcessor.collectedWarnings for the composition processor.
func (p *DefaultCompDiffProcessor) collectedWarnings() []dt.OutputWarning {
	if p.config.Warnings == nil {
		return nil
	}

	return p.config.Warnings.Warnings()
}
