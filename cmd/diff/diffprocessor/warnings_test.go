package diffprocessor

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/result"
	gcmp "github.com/google/go-cmp/cmp"
)

// TestAdvisoryCollector_Raise covers the dual emission and the collector's identity rule. Each raised
// advisory becomes a stderr line for humans AND a collected OutputWarning for structured output;
// Advisories returns them sorted by message, then context. An advisory identical to one already raised (same message AND same context) tells
// the user nothing new, so it is collapsed in BOTH channels. One whose context differs in any pair is
// a distinct occurrence and must still fire every time; the ownership-theft advisory in
// resource_manager.go depends on that, since it is raised once per composed resource with the
// resource's identity in its context.
func TestAdvisoryCollector_Raise(t *testing.T) {
	tests := map[string]struct {
		reason         string
		raise          []dt.OutputWarning
		wantStderr     string
		wantAdvisories []dt.OutputWarning
	}{
		"NoContext": {
			reason:         "A bare message renders as a bare WARNING line",
			raise:          []dt.OutputWarning{{Message: "something worth knowing"}},
			wantStderr:     "WARNING: something worth knowing\n",
			wantAdvisories: []dt.OutputWarning{{Message: "something worth knowing"}},
		},
		"ContextSortedInStderr": {
			reason: "Context pairs render in sorted key order, so repeated runs produce byte-identical stderr",
			raise: []dt.OutputWarning{{
				Message: "credentials missing",
				Context: map[string]string{"resource": "XR/one", "attempted": "2", "fetched": "1"},
			}},
			wantStderr: "WARNING: credentials missing (attempted=2, fetched=1, resource=XR/one)\n",
			wantAdvisories: []dt.OutputWarning{{
				Message: "credentials missing",
				Context: map[string]string{"resource": "XR/one", "attempted": "2", "fetched": "1"},
			}},
		},
		"IdenticalNoContextCollapses": {
			reason: "The same bare message repeated adds nothing, so only the first survives",
			raise: []dt.OutputWarning{
				{Message: "leftover containers"},
				{Message: "leftover containers"},
				{Message: "leftover containers"},
			},
			wantStderr:     "WARNING: leftover containers\n",
			wantAdvisories: []dt.OutputWarning{{Message: "leftover containers"}},
		},
		"IdenticalMessageAndContextCollapses": {
			reason: "The per-composition credential advisory repeats once per XR; all copies are identical",
			raise: []dt.OutputWarning{
				{Message: "credentials missing", Context: map[string]string{"composition": "xbuckets.example.org", "missing": "ns/a"}},
				{Message: "credentials missing", Context: map[string]string{"composition": "xbuckets.example.org", "missing": "ns/a"}},
			},
			wantStderr: "WARNING: credentials missing (composition=xbuckets.example.org, missing=ns/a)\n",
			wantAdvisories: []dt.OutputWarning{{
				Message: "credentials missing",
				Context: map[string]string{"composition": "xbuckets.example.org", "missing": "ns/a"},
			}},
		},
		"SameMessageDifferentContextBothFire": {
			reason: "The ownership advisory is per composed resource; collapsing it would hide resources",
			raise: []dt.OutputWarning{
				{Message: "assuming ownership", Context: map[string]string{"resource": "Bucket/one"}},
				{Message: "assuming ownership", Context: map[string]string{"resource": "Bucket/two"}},
			},
			wantStderr: "WARNING: assuming ownership (resource=Bucket/one)\n" +
				"WARNING: assuming ownership (resource=Bucket/two)\n",
			wantAdvisories: []dt.OutputWarning{
				{Message: "assuming ownership", Context: map[string]string{"resource": "Bucket/one"}},
				{Message: "assuming ownership", Context: map[string]string{"resource": "Bucket/two"}},
			},
		},
		"SameContextDifferentMessageBothFire": {
			reason: "Two different advisories about one resource are two facts",
			raise: []dt.OutputWarning{
				{Message: "first advisory", Context: map[string]string{"resource": "Bucket/one"}},
				{Message: "second advisory", Context: map[string]string{"resource": "Bucket/one"}},
			},
			wantStderr: "WARNING: first advisory (resource=Bucket/one)\n" +
				"WARNING: second advisory (resource=Bucket/one)\n",
			wantAdvisories: []dt.OutputWarning{
				{Message: "first advisory", Context: map[string]string{"resource": "Bucket/one"}},
				{Message: "second advisory", Context: map[string]string{"resource": "Bucket/one"}},
			},
		},
		"AdvisoriesSortedNotInRaisingOrder": {
			reason: "Concurrent diffs raise advisories in a scheduling-dependent order, so Advisories sorts by message, then context; stderr keeps raising order",
			raise: []dt.OutputWarning{
				{Message: "second advisory", Context: map[string]string{"resource": "Bucket/two"}},
				{Message: "second advisory", Context: map[string]string{"resource": "Bucket/one"}},
				{Message: "first advisory"},
			},
			wantStderr: "WARNING: second advisory (resource=Bucket/two)\n" +
				"WARNING: second advisory (resource=Bucket/one)\n" +
				"WARNING: first advisory\n",
			wantAdvisories: []dt.OutputWarning{
				{Message: "first advisory"},
				{Message: "second advisory", Context: map[string]string{"resource": "Bucket/one"}},
				{Message: "second advisory", Context: map[string]string{"resource": "Bucket/two"}},
			},
		},
		"ContextSplitCannotForgeAMatch": {
			reason: "A value containing the key/value separator must not collide with a differently split pair",
			raise: []dt.OutputWarning{
				{Message: "advisory", Context: map[string]string{"a": "b=c"}},
				{Message: "advisory", Context: map[string]string{"a=b": "c"}},
			},
			wantStderr: "WARNING: advisory (a=b=c)\n" +
				"WARNING: advisory (a=b=c)\n",
			wantAdvisories: []dt.OutputWarning{
				{Message: "advisory", Context: map[string]string{"a": "b=c"}},
				{Message: "advisory", Context: map[string]string{"a=b": "c"}},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer

			c := NewAdvisoryCollector(&stderr)

			for _, w := range tt.raise {
				c.Raise(w)
			}

			if diff := gcmp.Diff(tt.wantStderr, stderr.String()); diff != "" {
				t.Errorf("\n%s\nstderr mismatch (-want +got):\n%s", tt.reason, diff)
			}

			if diff := gcmp.Diff(tt.wantAdvisories, c.Advisories()); diff != "" {
				t.Errorf("\n%s\nadvisories mismatch (-want +got):\n%s", tt.reason, diff)
			}
		})
	}
}

// TestAdvisoryCollector_AdvisoriesIsACopy guards the documented copy semantics: a caller that retained
// an earlier result must not see it grow, or the renderer could serialize a slice being mutated.
func TestAdvisoryCollector_AdvisoriesIsACopy(t *testing.T) {
	c := NewAdvisoryCollector(nil)

	c.Raise(dt.OutputWarning{Message: "first"})

	snapshot := c.Advisories()

	c.Raise(dt.OutputWarning{Message: "second"})

	if len(snapshot) != 1 {
		t.Errorf("retained snapshot should still have 1 advisory, got %d", len(snapshot))
	}

	if got := len(c.Advisories()); got != 2 {
		t.Errorf("current advisories should have 2 entries, got %d", got)
	}
}

// TestAdvisoryCollector_ConcurrentRaise exercises the collector's mutex. Advisories are raised from
// clients and processors that a future change could parallelize, so the collector must not corrupt
// under concurrent use. Run with -race for this to be meaningful.
func TestAdvisoryCollector_ConcurrentRaise(t *testing.T) {
	var stderr bytes.Buffer

	c := NewAdvisoryCollector(&stderr)

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()

			c.Raise(dt.OutputWarning{Message: "concurrent advisory", Context: map[string]string{"worker": strconv.Itoa(i)}})
		}(i)
	}

	wg.Wait()

	if got := len(c.Advisories()); got != goroutines {
		t.Errorf("expected %d advisories, got %d", goroutines, got)
	}

	if got := strings.Count(stderr.String(), "WARNING: concurrent advisory"); got != goroutines {
		t.Errorf("expected %d stderr lines, got %d", goroutines, got)
	}
}
