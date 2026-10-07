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

package dockerrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

const (
	selfID  = "00000000000000ff"
	deadID  = "dead00000000000a"
	dead2ID = "dead00000000000b"
	liveID  = "1111111111111111"
)

// fakeDocker records every call a Run makes, in order, and ignores list filters: the Run must decide
// for itself which containers are its to remove, so a fake that pre-filters would hide exactly the bug
// these tests exist to catch.
type fakeDocker struct {
	containers       []container.Summary
	listErr          error
	removeErr        map[string]error // by container ID
	networkRemoveErr map[string]error // by network name
	networkCreateErr error

	calls []string
}

func (f *fakeDocker) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	f.record("list containers")
	return f.containers, f.listErr
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, opts container.RemoveOptions) error {
	if !opts.Force {
		f.record("remove container " + id + " without force")
	} else {
		f.record("remove container " + id)
	}

	return f.removeErr[id]
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.record("create network " + name + " " + opts.Driver + " " + LabelRunID + "=" + opts.Labels[LabelRunID])
	return network.CreateResponse{ID: "id-" + name}, f.networkCreateErr
}

func (f *fakeDocker) NetworkRemove(_ context.Context, name string) error {
	f.record("remove network " + name)
	return f.networkRemoveErr[name]
}

func (f *fakeDocker) Close() error {
	f.record("close")
	return nil
}

// advisories records Info messages: Info is the advisory level that becomes a user-facing warning.
type advisories struct {
	mu   sync.Mutex
	msgs []string
}

func (a *advisories) Info(msg string, _ ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.msgs = append(a.msgs, msg)
}

func (a *advisories) Debug(string, ...any)             {}
func (a *advisories) WithValues(...any) logging.Logger { return a }

func (a *advisories) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.msgs)
}

// fixture returns a container as the Docker API lists it: names carry a leading slash.
func fixture(id, name string, networks ...string) container.Summary {
	c := container.Summary{ID: id, NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{}}}
	if name != "" {
		c.Names = []string{"/" + name}
	}

	for _, n := range networks {
		c.NetworkSettings.Networks[n] = &network.EndpointSettings{}
	}

	return c
}

// hostContainers is a Docker host shared by a dead run, a live run, this run, and things crossplane-diff
// does not own.
var hostContainers = []container.Summary{
	// The dead run: a function container on its network, the render container that ran on that
	// network (unnamed, so it is recognised by network alone), and a function container started for
	// the local render engine (on no network of ours, so it is recognised by name alone).
	fixture("dead-fn", "function-go-templating-v0.11.0-diff-"+deadID, networkName(deadID)),
	fixture("dead-render", "modest_haibt", networkName(deadID)),
	fixture("dead-fn-local", "function-auto-ready-v0.4.2-diff-"+deadID, "bridge"),

	// A second dead run.
	fixture("dead2-fn", "function-auto-ready-v0.4.2-diff-"+dead2ID, networkName(dead2ID)),

	// A live run, and this run.
	fixture("live-fn", "function-go-templating-v0.11.0-diff-"+liveID, networkName(liveID)),
	fixture("live-render", "festive_curran", networkName(liveID)),
	fixture("self-fn", "function-go-templating-v0.11.0-diff-"+selfID, networkName(selfID)),

	// Not crossplane-diff's to judge: containers from versions that predate run ownership, another
	// tool's render containers, names that merely contain a run ID, and an unrelated container.
	fixture("legacy", "function-auto-ready-v0.4.2-comp-10cb1f47", "crossplane-render-dhpl9ndf"),
	fixture("other-tool", "function-auto-ready-v0.7.0-render", "crossplane-render"),
	fixture("id-not-suffix", deadID+"-diff-function", "bridge"),
	fixture("id-mid-name", "function-x-diff-"+deadID+"-copy", "bridge"),
	fixture("unrelated", "charming_elion", "host"),
	{ID: "no-network-settings", Names: []string{"/silly_ritchie"}},
}

// newTestRun returns a Run with a fixed ID whose leases live in dir.
func newTestRun(t *testing.T, dir string, docker *fakeDocker, ownsNetwork bool, log logging.Logger) *Run {
	t.Helper()

	r := New(dir, func() (Docker, error) { return docker, nil }, ownsNetwork, log)
	r.id = selfID

	return r
}

// holdLease takes the lease for id the way a running crossplane-diff does, releasing it at test end.
func holdLease(t *testing.T, dir, id string) {
	t.Helper()

	f, err := acquireLease(dir, id)
	if err != nil {
		t.Fatalf("acquireLease(%q): %v", id, err)
	}

	t.Cleanup(func() { _ = f.Close() })
}

// leaveLease leaves behind the lease file of a run that died without cleaning up: the file exists, but
// no process holds its lock.
func leaveLease(t *testing.T, dir, id string) {
	t.Helper()

	f, err := acquireLease(dir, id)
	if err != nil {
		t.Fatalf("acquireLease(%q): %v", id, err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close lease %q: %v", id, err)
	}
}

// dirEntries lists dir, so tests can assert which lease files survive.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// sortedFiles compares directory listings regardless of order. Call order, by contrast, is asserted
// exactly: containers must go before their network.
var sortedFiles = gcmp.FilterPath(func(p gcmp.Path) bool {
	return p.Last().String() == ".files"
}, cmpopts.SortSlices(func(a, b string) bool { return a < b }))

func TestRunStart(t *testing.T) {
	type want struct {
		calls      []string
		files      []string
		advisories int
	}

	tests := map[string]struct {
		reason string

		dead        []string // runs that died, leaving their lease behind
		live        []string // runs that are still running
		otherFiles  []string // files in the lease directory that are not leases
		docker      fakeDocker
		ownsNetwork bool

		want want
	}{
		"NothingLeftBehindMakesNoDockerCalls": {
			reason: "With no dead run to reclaim, starting a run must not cost a Docker call.",
			live:   []string{liveID},
			docker: fakeDocker{containers: hostContainers},
			want: want{
				files: []string{liveID + ".lock", selfID + ".lock"},
			},
		},
		"ReapsOnlyTheDeadRunsResources": {
			reason: "A dead run's containers — named, or on its network — are removed, then its network, then its " +
				"lease. A live run's, this run's and unowned containers are left alone.",
			dead:   []string{deadID},
			live:   []string{liveID},
			docker: fakeDocker{containers: hostContainers},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"remove container dead-fn-local",
					"remove network " + networkName(deadID),
				},
				files: []string{liveID + ".lock", selfID + ".lock"},
			},
		},
		"ReapsEveryDeadRun": {
			reason: "Every dead run is reclaimed, each one's containers before its network.",
			dead:   []string{deadID, dead2ID},
			docker: fakeDocker{containers: hostContainers},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"remove container dead-fn-local",
					"remove network " + networkName(deadID),
					"list containers",
					"remove container dead2-fn",
					"remove network " + networkName(dead2ID),
				},
				files: []string{selfID + ".lock"},
			},
		},
		"ContainerRemovalFailureKeepsNetworkAndLease": {
			reason: "A container that could not be removed may still hold the network, and the next run should " +
				"retry, so neither the network nor the lease is released. The failure is a warning, not an error.",
			dead: []string{deadID},
			docker: fakeDocker{
				containers: hostContainers,
				removeErr:  map[string]error{"dead-fn": errors.New("daemon says no")},
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"remove container dead-fn-local",
				},
				files:      []string{deadID + ".lock", selfID + ".lock"},
				advisories: 1,
			},
		},
		"NetworkRemovalFailureKeepsLease": {
			reason: "A network that could not be removed is retried by the next run.",
			dead:   []string{deadID},
			docker: fakeDocker{
				containers:       hostContainers,
				networkRemoveErr: map[string]error{networkName(deadID): errors.New("has active endpoints")},
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"remove container dead-fn-local",
					"remove network " + networkName(deadID),
				},
				files:      []string{deadID + ".lock", selfID + ".lock"},
				advisories: 1,
			},
		},
		"ResourcesAlreadyGoneCountAsReaped": {
			reason: "A container or network that no longer exists is already reclaimed, e.g. a run that used the " +
				"local render engine never created a network.",
			dead: []string{deadID},
			docker: fakeDocker{
				containers:       hostContainers,
				removeErr:        map[string]error{"dead-fn": cerrdefs.ErrNotFound},
				networkRemoveErr: map[string]error{networkName(deadID): cerrdefs.ErrNotFound},
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"remove container dead-fn-local",
					"remove network " + networkName(deadID),
				},
				files: []string{selfID + ".lock"},
			},
		},
		"ListFailureKeepsLeases": {
			reason: "If Docker cannot be listed nothing is removed and the next run retries.",
			dead:   []string{deadID},
			docker: fakeDocker{listErr: errors.New("cannot connect to the Docker daemon")},
			want: want{
				calls:      []string{"list containers"},
				files:      []string{deadID + ".lock", selfID + ".lock"},
				advisories: 1,
			},
		},
		"IgnoresFilesThatAreNotLeases": {
			reason: "Only <run ID>.lock files are leases; anything else in the directory is never acted on.",
			otherFiles: []string{
				"notes.txt",
				"not-a-run-id.lock",
				deadID + "-123.tmp",
			},
			docker: fakeDocker{containers: hostContainers},
			want: want{
				files: []string{deadID + "-123.tmp", "not-a-run-id.lock", "notes.txt", selfID + ".lock"},
			},
		},
		"CreatesItsLabelledNetworkAfterReaping": {
			reason: "A run that owns its render network creates it, labelled with its run ID, only after " +
				"reclaiming dead runs, so it cannot be mistaken for theirs.",
			dead:        []string{dead2ID},
			docker:      fakeDocker{containers: hostContainers},
			ownsNetwork: true,
			want: want{
				calls: []string{
					"list containers",
					"remove container dead2-fn",
					"remove network " + networkName(dead2ID),
					"create network " + networkName(selfID) + " bridge " + LabelRunID + "=" + selfID,
				},
				files: []string{selfID + ".lock"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			for _, id := range tt.dead {
				leaveLease(t, dir, id)
			}

			for _, id := range tt.live {
				holdLease(t, dir, id)
			}

			for _, f := range tt.otherFiles {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			log := &advisories{}
			r := newTestRun(t, dir, &tt.docker, tt.ownsNetwork, log)

			if err := r.Start(t.Context()); err != nil {
				t.Fatalf("\n%s\nStart(...): unexpected error: %v", tt.reason, err)
			}

			t.Cleanup(func() {
				if r.lease != nil {
					_ = r.lease.Close()
				}
			})

			got := want{calls: tt.docker.calls, files: dirEntries(t, dir), advisories: log.count()}
			if diff := gcmp.Diff(tt.want, got, gcmp.AllowUnexported(want{}), sortedFiles); diff != "" {
				t.Errorf("\n%s\nStart(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestRunLifecycle(t *testing.T) {
	type want struct {
		calls      []string
		files      []string
		advisories int
		startErr   bool
	}

	ownContainers := []container.Summary{
		fixture("self-fn", "function-go-templating-v0.11.0-diff-"+selfID, networkName(selfID)),
		fixture("self-render", "nervous_lewin", networkName(selfID)),
		fixture("self-fn-local", "function-auto-ready-v0.4.2-diff-"+selfID),
		fixture("live-fn", "function-go-templating-v0.11.0-diff-"+liveID, networkName(liveID)),
		fixture("unrelated", "charming_elion", "host"),
	}

	tests := map[string]struct {
		reason string

		docker fakeDocker
		start  bool // whether the run ever started

		want want
	}{
		"NeverStartedTouchesNothing": {
			reason: "A run that never rendered created nothing, so closing it makes no Docker call.",
			docker: fakeDocker{containers: ownContainers},
			want:   want{files: []string{}},
		},
		"CloseRemovesOwnContainersThenNetworkThenLease": {
			reason: "Closing sweeps everything the run started, including what upstream cleanup left behind, " +
				"removes its network once nothing is attached, then gives up its lease.",
			docker: fakeDocker{containers: ownContainers},
			start:  true,
			want: want{
				calls: []string{
					"create network " + networkName(selfID) + " bridge " + LabelRunID + "=" + selfID,
					"list containers",
					"remove container self-fn",
					"remove container self-render",
					"remove container self-fn-local",
					"remove network " + networkName(selfID),
					"close",
				},
				files: []string{},
			},
		},
		"FailedSweepLeavesLeaseForTheNextRun": {
			reason: "If the run cannot remove its own containers, its lease stays behind (unlocked) so the next " +
				"run reclaims them.",
			docker: fakeDocker{
				containers: ownContainers,
				removeErr:  map[string]error{"self-render": errors.New("removal already in progress")},
			},
			start: true,
			want: want{
				calls: []string{
					"create network " + networkName(selfID) + " bridge " + LabelRunID + "=" + selfID,
					"list containers",
					"remove container self-fn",
					"remove container self-render",
					"remove container self-fn-local",
					"close",
				},
				files:      []string{selfID + ".lock"},
				advisories: 1,
			},
		},
		"NetworkCreateFailureFailsStartButStillCleansUp": {
			reason: "Rendering cannot proceed without the network, so Start fails; Close still releases the lease.",
			docker: fakeDocker{networkCreateErr: errors.New("all predefined address pools have been fully subnetted")},
			start:  true,
			want: want{
				calls: []string{
					"create network " + networkName(selfID) + " bridge " + LabelRunID + "=" + selfID,
					"list containers",
					"remove network " + networkName(selfID),
					"close",
				},
				files:    []string{},
				startErr: true,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			log := &advisories{}
			r := newTestRun(t, dir, &tt.docker, true, log)

			var startErr error
			if tt.start {
				// Starting twice is starting once: the engine calls Start for every new batch of functions.
				startErr = r.Start(t.Context())
				if startErr == nil {
					startErr = r.Start(t.Context())
				}
			}

			r.Close(t.Context())
			// Closing twice is closing once: the processor's Cleanup runs before rendering and again on exit.
			r.Close(t.Context())

			got := want{calls: tt.docker.calls, files: dirEntries(t, dir), advisories: log.count(), startErr: startErr != nil}
			if diff := gcmp.Diff(tt.want, got, gcmp.AllowUnexported(want{}), sortedFiles); diff != "" {
				t.Errorf("\n%s\nStart(...)/Close(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

// TestRunLeaseIsHeldWhileStarted pins the liveness signal itself: while a run is started, no other
// process (or other Run in this process) can take its lease, so no reaper treats it as dead; once the
// run is gone, its lease is free.
func TestRunLeaseIsHeldWhileStarted(t *testing.T) {
	dir := t.TempDir()
	r := newTestRun(t, dir, &fakeDocker{}, false, &advisories{})

	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start(...): %v", err)
	}

	path := leasePath(dir, selfID)

	if f, ok := tryLease(path); ok {
		_ = f.Close()

		t.Fatal("tryLease(...) took the lease of a started run: a reaper would treat a live run as dead")
	}

	// Simulate the process dying: the kernel drops the lock, the file stays.
	if err := r.lease.Close(); err != nil {
		t.Fatal(err)
	}

	f, ok := tryLease(path)
	if !ok {
		t.Fatal("tryLease(...) could not take the lease of a dead run: its resources would never be reclaimed")
	}

	_ = f.Close()
}

func TestRunNames(t *testing.T) {
	r := New(t.TempDir(), func() (Docker, error) { return &fakeDocker{}, nil }, false, &advisories{})

	if !validRunID(r.id) {
		t.Fatalf("New(...) generated run ID %q, want %d lowercase hex characters", r.id, runIDLength)
	}

	if got, want := r.NetworkName(), "crossplane-diff-"+r.id; got != want {
		t.Errorf("NetworkName() = %q, want %q", got, want)
	}

	if got := r.ContainerName("xpkg.io/crossplane-contrib/function-go-templating:v0.11.0"); !strings.HasSuffix(got, "-diff-"+r.id) {
		t.Errorf("ContainerName(...) = %q, want suffix %q", got, "-diff-"+r.id)
	}

	if other := New(t.TempDir(), nil, false, &advisories{}); other.id == r.id {
		t.Errorf("New(...) generated the same run ID %q twice", r.id)
	}
}
