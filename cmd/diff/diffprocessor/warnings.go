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
)

// AdvisoryCollector is the CLI's dt.Advisories: every advisory raised is written to
// stderr immediately, as a WARNING line, and collected for structured output's warnings[].
//
// # Why a channel of its own, not the logger
//
// Advisories used to be raised as logging.Logger Info calls, with a logger decorator turning every
// Info into a warning. That made ordinary info logging and user-facing advisories one method, so a
// hosting command could not tell them apart, and a library given the logger could raise warnings
// nobody chose. An advisory is now a typed value raised on this channel, which components receive
// alongside their logger; the logger is for tracing only.
//
// The user-facing label is WARNING rather than INFO: an operator reading "INFO: applying this diff
// will assume ownership" would under-weight it.
//
// # Why it writes to stderr itself
//
// Owning the human rendering here means one mechanism produces both views, the same dual-emission
// contract OutputError follows, and the text stays fit for a CLI rather than taking a logger's
// development-mode format (timestamp, level, caller).
//
// # Why identical advisories are collapsed
//
// An advisory is raised where the condition is discovered, and several of those sites sit inside loops
// the caller chose: resolveFunctionCredentials runs once per XR, so a single unfetchable secret on a
// composition affecting thirty XRs would otherwise produce thirty byte-identical WARNING lines and
// thirty identical warnings[] entries. A repeat that is identical in BOTH message and context carries
// no information the user has not already been given, so the collector keeps only the first. Advisories
// whose context differs in any pair are distinct occurrences and every one of them is kept: the
// ownership-theft advisory in resource_manager.go names the composed resource in its context and must
// fire once per resource. So a raising site puts what distinguishes one occurrence from another in the
// context, which it should be doing regardless, since context is what a machine consumer reads.
//
// It is safe for concurrent use. Construct one with NewAdvisoryCollector.
type AdvisoryCollector struct {
	mu         sync.Mutex
	stderr     io.Writer
	advisories []dt.OutputWarning
	// seen holds the identity of every advisory already recorded, so an identical repeat is dropped
	// from both channels rather than re-emitted. Keyed by warningKey.
	seen map[string]struct{}
}

// NewAdvisoryCollector returns a collector that writes each advisory to stderr and collects it for
// structured output. A nil stderr only collects.
func NewAdvisoryCollector(stderr io.Writer) *AdvisoryCollector {
	return &AdvisoryCollector{stderr: stderr, seen: map[string]struct{}{}}
}

// Raise records w: written to stderr now, and collected for structured output. An advisory identical
// to one already raised (same message and same context) is dropped from both channels; see the type
// comment.
func (c *AdvisoryCollector) Raise(w dt.OutputWarning) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := warningKey(w)
	if _, repeat := c.seen[key]; repeat {
		return
	}

	c.seen[key] = struct{}{}
	c.advisories = append(c.advisories, w)

	if c.stderr != nil {
		// A failed write to stderr is not worth failing a diff over, and there is nowhere useful to
		// report it: the reporting channel is the thing that just failed.
		_, _ = fmt.Fprintln(c.stderr, w.FormatWarning())
	}
}

// Advisories returns the advisories raised so far, sorted by message and then context (by warningKey),
// so the order does not depend on the order concurrent work raised them in. The returned slice is a
// copy, so callers may retain it while more are raised.
func (c *AdvisoryCollector) Advisories() []dt.OutputWarning {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.advisories) == 0 {
		return nil
	}

	out := make([]dt.OutputWarning, len(c.advisories))
	copy(out, c.advisories)
	slices.SortFunc(out, func(a, b dt.OutputWarning) int { return strings.Compare(warningKey(a), warningKey(b)) })

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
