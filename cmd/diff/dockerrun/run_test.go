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
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	gcmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

const (
	selfID  = "00000000000000ff"
	deadID  = "dead00000000000a"
	dead2ID = "dead00000000000b"
	liveID  = "1111111111111111"
)

// fakeDocker records every call a Run makes, in order. It applies list filters the way the Docker daemon
// does (a resource matches only if it carries every label=key=value pair, and any other filter term
// fails the test), because the filter is what decides which resources are the Run's: the fixtures
// below hold resources that must not match, so a wrong filter removes them.
type fakeDocker struct {
	t *testing.T

	containers       []container.Summary
	networks         []network.Summary
	listErr          error
	removeErr        map[string]error // by container ID
	networkListErr   error
	networkRemoveErr map[string]error // by network ID

	calls []string
}

func (f *fakeDocker) record(call string) {
	f.calls = append(f.calls, call)
}

// matches reports whether labels satisfy filters as the Docker daemon's MatchKVList does.
func (f *fakeDocker) matches(filters client.Filters, labels map[string]string) bool {
	for term, values := range filters {
		if term != "label" {
			f.t.Errorf("unexpected Docker list filter term %q", term)
			return false
		}

		for v := range values {
			k, want, hasValue := strings.Cut(v, "=")
			got, ok := labels[k]

			if !ok || (hasValue && got != want) {
				return false
			}
		}
	}

	return true
}

func (f *fakeDocker) ContainerList(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	f.record("list containers")

	if !opts.All {
		f.t.Error("ContainerList without All would miss stopped containers")
	}

	var res client.ContainerListResult

	for _, c := range f.containers {
		if f.matches(opts.Filters, c.Labels) {
			res.Items = append(res.Items, c)
		}
	}

	return res, f.listErr
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if !opts.Force {
		f.record("remove container " + id + " without force")
	} else {
		f.record("remove container " + id)
	}

	return client.ContainerRemoveResult{}, f.removeErr[id]
}

func (f *fakeDocker) NetworkList(_ context.Context, opts client.NetworkListOptions) (client.NetworkListResult, error) {
	f.record("list networks")

	var res client.NetworkListResult

	for _, n := range f.networks {
		if f.matches(opts.Filters, n.Labels) {
			res.Items = append(res.Items, n)
		}
	}

	return res, f.networkListErr
}

func (f *fakeDocker) NetworkRemove(_ context.Context, id string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.record("remove network " + id)
	return client.NetworkRemoveResult{}, f.networkRemoveErr[id]
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

// session returns the labels the upstream render engine puts on what it creates for session id.
func session(id string) map[string]string {
	return map[string]string{render.LabelKeyManagedBy: render.LabelValueManagedByCrossplane, render.LabelKeySession: id}
}

// ctr returns a container as the Docker API lists it: names carry a leading slash.
func ctr(id, name string, labels map[string]string) container.Summary {
	return container.Summary{ID: id, Names: []string{"/" + name}, Labels: labels}
}

func netw(id, name string, labels map[string]string) network.Summary {
	return network.Summary{Network: network.Network{ID: id, Name: name, Labels: labels}}
}

// hostContainers is a Docker host shared by a dead run, a live run, this run, and things crossplane-diff
// does not own.
var hostContainers = []container.Summary{
	// The dead run: a function container and the render container that was running when it died.
	ctr("dead-fn", "festive_curran", session(deadID)),
	ctr("dead-render", "modest_haibt", session(deadID)),

	// A second dead run.
	ctr("dead2-fn", "silly_ritchie", session(dead2ID)),

	// A live run, and this run.
	ctr("live-fn", "nervous_lewin", session(liveID)),
	ctr("self-fn", "charming_elion", session(selfID)),

	// Not crossplane-diff's to judge: a container from a version that named function containers for
	// their run instead of labelling them, `crossplane render` run by hand (its own session), a
	// container that claims the session but not crossplane's management, and an unrelated container.
	ctr("legacy", "function-go-templating-v0.11.0-diff-"+deadID, nil),
	ctr("other-render", "function-auto-ready", session("a1b2c3d4e5f6")),
	ctr("session-only", "focused_turing", map[string]string{render.LabelKeySession: deadID}),
	ctr("unrelated", "vibrant_hopper", map[string]string{"app": "db"}),
}

// hostNetworks is the same host's networks.
var hostNetworks = []network.Summary{
	netw("dead-net", "crossplane-render-dhpl9ndf", session(deadID)),
	netw("dead2-net", "crossplane-render-x7k2m9qp", session(dead2ID)),
	netw("live-net", "crossplane-render-q4w8e2rt", session(liveID)),
	netw("self-net", "crossplane-render-z9y8x7w6", session(selfID)),

	// Not crossplane-diff's to judge: a network from a version that named the network for its run, one
	// the user supplied with CROSSPLANE_DIFF_DOCKER_NETWORK (render never labels it), and one that
	// claims the session but not crossplane's management.
	netw("legacy-net", "crossplane-diff-"+deadID, map[string]string{"crossplane-diff.io/run-id": deadID}),
	netw("user-net", "github_network_123", nil),
	netw("session-only-net", "lookalike", map[string]string{render.LabelKeySession: deadID}),
}

// newTestRun returns a Run with a fixed ID whose leases live in dir.
func newTestRun(t *testing.T, dir string, docker *fakeDocker, log logging.Logger) *Run {
	t.Helper()

	docker.t = t

	r := New(dir, func() (Docker, error) { return docker, nil }, log)
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

	host := fakeDocker{containers: hostContainers, networks: hostNetworks}

	tests := map[string]struct {
		reason string

		dead       []string // runs that died, leaving their lease behind
		live       []string // runs that are still running
		otherFiles []string // files in the lease directory that are not leases
		docker     fakeDocker

		want want
	}{
		"NothingLeftBehindMakesNoDockerCalls": {
			reason: "With no dead run to reclaim, starting a run must not cost a Docker call.",
			live:   []string{liveID},
			docker: host,
			want: want{
				files: []string{liveID + ".lock", selfID + ".lock"},
			},
		},
		"ReapsOnlyTheDeadRunsResources": {
			reason: "A dead run's containers are removed, then its network, then its lease. A live run's, this " +
				"run's, and anything without both of the dead run's labels (including resources named for it by " +
				"earlier versions) are left alone.",
			dead:   []string{deadID},
			live:   []string{liveID},
			docker: host,
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"list networks",
					"remove network dead-net",
				},
				files: []string{liveID + ".lock", selfID + ".lock"},
			},
		},
		"ReapsEveryDeadRun": {
			reason: "Every dead run is reclaimed, each one's containers before its network.",
			dead:   []string{deadID, dead2ID},
			docker: host,
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"list networks",
					"remove network dead-net",
					"list containers",
					"remove container dead2-fn",
					"list networks",
					"remove network dead2-net",
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
				networks:   hostNetworks,
				removeErr:  map[string]error{"dead-fn": errors.New("daemon says no")},
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
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
				networks:         hostNetworks,
				networkRemoveErr: map[string]error{"dead-net": errors.New("has active endpoints")},
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"list networks",
					"remove network dead-net",
				},
				files:      []string{deadID + ".lock", selfID + ".lock"},
				advisories: 1,
			},
		},
		"ResourcesAlreadyGoneCountAsReaped": {
			reason: "A container or network that no longer exists, e.g. removed by a concurrent reaper or by " +
				"Docker itself between the list and the remove, is already reclaimed.",
			dead: []string{deadID},
			docker: fakeDocker{
				containers:       hostContainers,
				networks:         hostNetworks,
				removeErr:        map[string]error{"dead-fn": cerrdefs.ErrNotFound},
				networkRemoveErr: map[string]error{"dead-net": cerrdefs.ErrNotFound},
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"list networks",
					"remove network dead-net",
				},
				files: []string{selfID + ".lock"},
			},
		},
		"ContainerListFailureKeepsLeases": {
			reason: "If Docker cannot be listed nothing is removed and the next run retries.",
			dead:   []string{deadID},
			docker: fakeDocker{listErr: errors.New("cannot connect to the Docker daemon")},
			want: want{
				calls:      []string{"list containers"},
				files:      []string{deadID + ".lock", selfID + ".lock"},
				advisories: 1,
			},
		},
		"NetworkListFailureKeepsLeases": {
			reason: "If networks cannot be listed the lease is kept, so the next run removes the network.",
			dead:   []string{deadID},
			docker: fakeDocker{
				containers:     hostContainers,
				networks:       hostNetworks,
				networkListErr: errors.New("cannot connect to the Docker daemon"),
			},
			want: want{
				calls: []string{
					"list containers",
					"remove container dead-fn",
					"remove container dead-render",
					"list networks",
				},
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
			docker: host,
			want: want{
				files: []string{deadID + "-123.tmp", "not-a-run-id.lock", "notes.txt", selfID + ".lock"},
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
			r := newTestRun(t, dir, &tt.docker, log)

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
	}

	ownContainers := []container.Summary{
		ctr("self-fn", "festive_curran", session(selfID)),
		ctr("self-render", "nervous_lewin", session(selfID)),
		ctr("live-fn", "modest_haibt", session(liveID)),
		ctr("legacy", "function-go-templating-v0.11.0-diff-"+selfID, nil),
		ctr("unrelated", "charming_elion", nil),
	}

	ownNetworks := []network.Summary{
		netw("self-net", "crossplane-render-z9y8x7w6", session(selfID)),
		netw("live-net", "crossplane-render-q4w8e2rt", session(liveID)),
		netw("user-net", "github_network_123", nil),
	}

	tests := map[string]struct {
		reason string

		docker fakeDocker
		start  bool // whether the run ever started

		want want
	}{
		"NeverStartedTouchesNothing": {
			reason: "A run that never rendered created nothing, so closing it makes no Docker call; the engine's " +
				"own cleanup still runs.",
			docker: fakeDocker{containers: ownContainers, networks: ownNetworks},
			want:   want{calls: []string{"engine cleanup", "engine cleanup"}, files: []string{}},
		},
		"CloseRemovesOwnContainersThenEngineCleanupThenNetworksThenLease": {
			reason: "Closing sweeps every container labelled with the run's session, including what upstream " +
				"cleanup left behind, lets the engine remove its network once nothing is attached, removes any " +
				"of the run's networks still left, then gives up its lease. A network the user supplied carries " +
				"no session label, so it is never removed.",
			docker: fakeDocker{containers: ownContainers, networks: ownNetworks},
			start:  true,
			want: want{
				calls: []string{
					"list containers",
					"remove container self-fn",
					"remove container self-render",
					"engine cleanup",
					"list networks",
					"remove network self-net",
					"close",
					"engine cleanup",
				},
				files: []string{},
			},
		},
		"EngineAlreadyRemovedItsNetwork": {
			reason: "When the engine's cleanup removed its network, the network may still be listed, or gone by the " +
				"time it is removed. Either way it is reclaimed.",
			docker: fakeDocker{
				containers:       ownContainers,
				networks:         ownNetworks,
				networkRemoveErr: map[string]error{"self-net": cerrdefs.ErrNotFound},
			},
			start: true,
			want: want{
				calls: []string{
					"list containers",
					"remove container self-fn",
					"remove container self-render",
					"engine cleanup",
					"list networks",
					"remove network self-net",
					"close",
					"engine cleanup",
				},
				files: []string{},
			},
		},
		"FailedSweepLeavesLeaseForTheNextRun": {
			reason: "If the run cannot remove its own containers, its lease stays behind (unlocked) so the next " +
				"run reclaims them. The engine's cleanup still runs.",
			docker: fakeDocker{
				containers: ownContainers,
				networks:   ownNetworks,
				removeErr:  map[string]error{"self-render": errors.New("removal already in progress")},
			},
			start: true,
			want: want{
				calls: []string{
					"list containers",
					"remove container self-fn",
					"remove container self-render",
					"engine cleanup",
					"close",
					"engine cleanup",
				},
				files:      []string{selfID + ".lock"},
				advisories: 1,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			log := &advisories{}
			r := newTestRun(t, dir, &tt.docker, log)

			if tt.start {
				// Starting twice is starting once: the engine calls Start for every new batch of functions.
				for range 2 {
					if err := r.Start(t.Context()); err != nil {
						t.Fatalf("\n%s\nStart(...): unexpected error: %v", tt.reason, err)
					}
				}
			}

			release := func() { tt.docker.record("engine cleanup") }

			r.Close(t.Context(), release)
			// Closing twice is closing once: the processor's Cleanup runs before rendering and again on exit.
			r.Close(t.Context(), release)

			got := want{calls: tt.docker.calls, files: dirEntries(t, dir), advisories: log.count()}
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
	r := newTestRun(t, dir, &fakeDocker{}, &advisories{})

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

func TestRunID(t *testing.T) {
	r := New(t.TempDir(), nil, &advisories{})

	if !validRunID(r.ID()) {
		t.Fatalf("New(...) generated run ID %q, want %d lowercase hex characters", r.ID(), runIDLength)
	}

	if other := New(t.TempDir(), nil, &advisories{}); other.ID() == r.ID() {
		t.Errorf("New(...) generated the same run ID %q twice", r.ID())
	}
}
