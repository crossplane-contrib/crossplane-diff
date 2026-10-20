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
	"os"
	"path/filepath"
	"slices"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/result"
	tu "github.com/crossplane-contrib/crossplane-diff/cmd/diff/testutils"
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

const (
	running = container.StateRunning
	exited  = container.StateExited
)

// hostContainers is a Docker host shared by two dead runs, a live run, this run, and things crossplane-diff
// does not own. A function container that already stopped is listed only with All, and one that is still
// running is removed only with Force, so either mistake leaves one behind. Each run's containers are
// attached to its network, which therefore cannot go while they are there.
var hostContainers = []container.Summary{
	// The dead run: a stopped function container and the render container that was running when it died.
	tu.DockerContainer("dead-fn", exited, tu.DockerSession(deadID), "dead-net"),
	tu.DockerContainer("dead-render", running, tu.DockerSession(deadID), "dead-net"),

	// A second dead run.
	tu.DockerContainer("dead2-fn", exited, tu.DockerSession(dead2ID), "dead2-net"),

	// A live run, and this run.
	tu.DockerContainer("live-fn", running, tu.DockerSession(liveID), "live-net"),
	tu.DockerContainer("self-fn", running, tu.DockerSession(selfID), "self-net"),

	// Not crossplane-diff's to judge: a container from a version that named function containers for their
	// run instead of labelling them, `crossplane render` run by hand (its own session), a container that
	// claims the dead run's session but not crossplane's management, and an unrelated container.
	tu.DockerContainer("legacy", running, nil, "legacy-net"),
	tu.DockerContainer("other-render", running, tu.DockerSession("a1b2c3d4e5f6")),
	tu.DockerContainer("session-only", running, map[string]string{render.LabelKeySession: deadID}),
	tu.DockerContainer("unrelated", running, map[string]string{"app": "db"}),
}

// hostNetworks is the same host's networks.
var hostNetworks = []network.Summary{
	tu.DockerNetwork("dead-net", tu.DockerSession(deadID)),
	tu.DockerNetwork("dead2-net", tu.DockerSession(dead2ID)),
	tu.DockerNetwork("live-net", tu.DockerSession(liveID)),
	tu.DockerNetwork("self-net", tu.DockerSession(selfID)),

	// Not crossplane-diff's to judge: a network from a version that named the network for its run, one the
	// user supplied with CROSSPLANE_DIFF_DOCKER_NETWORK (render never labels it), and one that claims the
	// dead run's session but not crossplane's management.
	tu.DockerNetwork("legacy-net", map[string]string{"crossplane-diff.io/run-id": deadID}),
	tu.DockerNetwork("user-net", nil),
	tu.DockerNetwork("session-only-net", map[string]string{render.LabelKeySession: deadID}),
}

var (
	// untouchedContainers and untouchedNetworks are what reaping dead runs never removes.
	untouchedContainers = []string{"legacy", "live-fn", "other-render", "self-fn", "session-only", "unrelated"}
	untouchedNetworks   = []string{"legacy-net", "live-net", "self-net", "session-only-net", "user-net"}

	allContainers = slices.Concat(untouchedContainers, []string{"dead-fn", "dead-render", "dead2-fn"})
	allNetworks   = slices.Concat(untouchedNetworks, []string{"dead-net", "dead2-net"})
)

// newRun returns a Run with ID runID whose leases live in dir.
func newRun(dir, runID string, docker *tu.FakeDocker, advisories dt.Advisories) *Run {
	r := New(dir, func() (Docker, error) { return docker, nil }, logging.NewNopLogger(), advisories)
	r.id = runID

	return r
}

// start starts r the way a running crossplane-diff does, holding its lease until the test ends.
func start(t *testing.T, r *Run) {
	t.Helper()

	if err := r.Start(t.Context()); err != nil {
		t.Fatalf("Start(...) of run %s: unexpected error: %v", r.ID(), err)
	}

	t.Cleanup(func() {
		if r.lease != nil {
			_ = r.lease.Close()
		}
	})
}

// leaveLease leaves behind the lease file of a run that died without cleaning up: the file exists, but no
// process holds its lock.
func leaveLease(t *testing.T, dir, runID string) {
	t.Helper()

	f, err := acquireLease(dir, runID)
	if err != nil {
		t.Fatalf("acquireLease(%q): %v", runID, err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close lease %q: %v", runID, err)
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

// unordered compares what is left regardless of order: only the end state matters.
var unordered = []gcmp.Option{
	cmpopts.SortSlices(func(a, b string) bool { return a < b }),
	cmpopts.EquateEmpty(),
}

func TestRunStart(t *testing.T) {
	// want is what is left once the run has started: the host's containers and networks, the files in the
	// lease directory, and how many warnings the run raised.
	type want struct {
		Containers []string
		Networks   []string
		Files      []string
		Warnings   int
	}

	tests := map[string]struct {
		reason string

		dead       []string      // runs that died, leaving their lease behind
		live       []string      // runs that are still running
		otherFiles []string      // files in the lease directory that are not leases
		docker     tu.FakeDocker // failures to inject; the host is hostContainers and hostNetworks

		want want
	}{
		"NoDeadRunRemovesNothing": {
			reason: "With no dead run to reclaim, starting a run removes nothing.",
			live:   []string{liveID},
			want: want{
				Containers: allContainers,
				Networks:   allNetworks,
				Files:      []string{liveID + ".lock", selfID + ".lock"},
			},
		},
		"ReapsOnlyTheDeadRunsResources": {
			reason: "A dead run's containers, network and lease are removed. A live run's, this run's, a run " +
				"whose lease is not in this directory (another user's), and anything without both of the dead " +
				"run's labels (including resources named for it by earlier versions) are left alone.",
			dead: []string{deadID},
			live: []string{liveID},
			want: want{
				Containers: slices.Concat(untouchedContainers, []string{"dead2-fn"}),
				Networks:   slices.Concat(untouchedNetworks, []string{"dead2-net"}),
				Files:      []string{liveID + ".lock", selfID + ".lock"},
			},
		},
		"ReapsEveryDeadRun": {
			reason: "Every dead run is reclaimed.",
			dead:   []string{deadID, dead2ID},
			want: want{
				Containers: untouchedContainers,
				Networks:   untouchedNetworks,
				Files:      []string{selfID + ".lock"},
			},
		},
		"ContainerRemovalFailureKeepsNetworkAndLease": {
			reason: "A container that could not be removed still holds its network, and the next run should " +
				"retry, so the lease is kept. The failure is a warning, not an error.",
			dead:   []string{deadID},
			docker: tu.FakeDocker{RemoveErr: map[string]error{"dead-fn": errors.New("daemon says no")}},
			want: want{
				Containers: slices.Concat(untouchedContainers, []string{"dead-fn", "dead2-fn"}),
				Networks:   allNetworks,
				Files:      []string{deadID + ".lock", selfID + ".lock"},
				Warnings:   1,
			},
		},
		"NetworkRemovalFailureKeepsLease": {
			reason: "A network that could not be removed is retried by the next run.",
			dead:   []string{deadID},
			docker: tu.FakeDocker{RemoveErr: map[string]error{"dead-net": errors.New("daemon says no")}},
			want: want{
				Containers: slices.Concat(untouchedContainers, []string{"dead2-fn"}),
				Networks:   allNetworks,
				Files:      []string{deadID + ".lock", selfID + ".lock"},
				Warnings:   1,
			},
		},
		"ResourcesAlreadyGoneCountAsReaped": {
			reason: "A container or network that no longer exists by the time it is removed, e.g. removed by a " +
				"concurrent reaper, is already reclaimed.",
			dead:   []string{deadID},
			docker: tu.FakeDocker{Vanished: map[string]bool{"dead-fn": true, "dead-net": true}},
			want: want{
				Containers: slices.Concat(untouchedContainers, []string{"dead2-fn"}),
				Networks:   slices.Concat(untouchedNetworks, []string{"dead2-net"}),
				Files:      []string{selfID + ".lock"},
			},
		},
		"ContainerListFailureKeepsLeases": {
			reason: "If containers cannot be listed nothing is removed and the next run retries.",
			dead:   []string{deadID},
			docker: tu.FakeDocker{ListErr: errors.New("cannot connect to the Docker daemon")},
			want: want{
				Containers: allContainers,
				Networks:   allNetworks,
				Files:      []string{deadID + ".lock", selfID + ".lock"},
				Warnings:   1,
			},
		},
		"NetworkListFailureKeepsLeases": {
			reason: "If networks cannot be listed the lease is kept, so the next run removes the network.",
			dead:   []string{deadID},
			docker: tu.FakeDocker{NetworkListErr: errors.New("cannot connect to the Docker daemon")},
			want: want{
				Containers: slices.Concat(untouchedContainers, []string{"dead2-fn"}),
				Networks:   allNetworks,
				Files:      []string{deadID + ".lock", selfID + ".lock"},
				Warnings:   1,
			},
		},
		"IgnoresFilesThatAreNotLeases": {
			reason: "Only <run ID>.lock files are leases; anything else in the directory is never acted on.",
			otherFiles: []string{
				"notes.txt",
				"not-a-run-id.lock",
				deadID + "-123.tmp",
			},
			want: want{
				Containers: allContainers,
				Networks:   allNetworks,
				Files:      []string{deadID + "-123.tmp", "not-a-run-id.lock", "notes.txt", selfID + ".lock"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			docker := tt.docker
			docker.Containers = slices.Clone(hostContainers)
			docker.Networks = slices.Clone(hostNetworks)

			// Live runs started before any run died, so they had nothing to reclaim themselves.
			for _, runID := range tt.live {
				start(t, newRun(dir, runID, &docker, &tu.AdvisoryRecorder{}))
			}

			for _, runID := range tt.dead {
				leaveLease(t, dir, runID)
			}

			for _, f := range tt.otherFiles {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			advisories := &tu.AdvisoryRecorder{}
			start(t, newRun(dir, selfID, &docker, advisories))

			got := want{
				Containers: docker.ContainerIDs(),
				Networks:   docker.NetworkNames(),
				Files:      dirEntries(t, dir),
				Warnings:   len(advisories.Advisories()),
			}
			if diff := gcmp.Diff(tt.want, got, unordered...); diff != "" {
				t.Errorf("\n%s\nStart(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

func TestRunClose(t *testing.T) {
	// want is what is left once the run has closed, and how the engine's own cleanup went.
	type want struct {
		Containers []string
		Networks   []string
		Files      []string
		Warnings   int

		// EngineCleanedUp is whether the engine's cleanup ran, and EngineCleanupFailed whether it could not
		// remove its network.
		EngineCleanedUp     bool
		EngineCleanupFailed bool
	}

	// The run's function and render containers are attached to the network the engine created for it.
	ownContainers := []container.Summary{
		tu.DockerContainer("self-fn", running, tu.DockerSession(selfID), "self-net"),
		tu.DockerContainer("self-render", running, tu.DockerSession(selfID), "self-net"),
		tu.DockerContainer("live-fn", running, tu.DockerSession(liveID), "live-net"),
		tu.DockerContainer("legacy", running, nil),
		tu.DockerContainer("unrelated", running, nil),
	}

	ownNetworks := []network.Summary{
		tu.DockerNetwork("self-net", tu.DockerSession(selfID)),
		tu.DockerNetwork("live-net", tu.DockerSession(liveID)),
		tu.DockerNetwork("user-net", nil),
	}

	tests := map[string]struct {
		reason string

		started bool          // whether the run ever started
		docker  tu.FakeDocker // the host
		// engineRemovesNetwork is whether the engine's cleanup removes the network it created, as it does
		// unless it fails.
		engineRemovesNetwork bool

		want want
	}{
		"NeverStartedRemovesNothing": {
			reason: "A run that never rendered created nothing, so closing it removes nothing and leaves no " +
				"lease. The engine's own cleanup still runs.",
			docker: tu.FakeDocker{
				Containers: []container.Summary{tu.DockerContainer("live-fn", running, tu.DockerSession(liveID), "live-net")},
				Networks:   []network.Summary{tu.DockerNetwork("live-net", tu.DockerSession(liveID))},
			},
			want: want{Containers: []string{"live-fn"}, Networks: []string{"live-net"}, EngineCleanedUp: true},
		},
		"RemovesEverythingTheRunCreated": {
			reason: "Closing removes every container labelled with the run's session before the engine's cleanup " +
				"runs, so the engine can remove its network, then gives up its lease. A network the user supplied " +
				"carries no session label, so it is never removed.",
			started:              true,
			docker:               tu.FakeDocker{Containers: ownContainers, Networks: ownNetworks},
			engineRemovesNetwork: true,
			want: want{
				Containers:      []string{"legacy", "live-fn", "unrelated"},
				Networks:        []string{"live-net", "user-net"},
				EngineCleanedUp: true,
			},
		},
		"RemovesTheNetworkTheEngineLeft": {
			reason:  "A network of the run that the engine's cleanup left behind is removed too.",
			started: true,
			docker:  tu.FakeDocker{Containers: ownContainers, Networks: ownNetworks},
			want: want{
				Containers:      []string{"legacy", "live-fn", "unrelated"},
				Networks:        []string{"live-net", "user-net"},
				EngineCleanedUp: true,
			},
		},
		"FailedContainerRemovalLeavesLeaseForTheNextRun": {
			reason: "If the run cannot remove one of its containers, its network cannot go either, and its lease " +
				"stays behind (unlocked) so the next run reclaims both. Closing again does not warn again.",
			started: true,
			docker: tu.FakeDocker{
				Containers: ownContainers,
				Networks:   ownNetworks,
				RemoveErr:  map[string]error{"self-render": errors.New("removal already in progress")},
			},
			engineRemovesNetwork: true,
			want: want{
				Containers:          []string{"legacy", "live-fn", "self-render", "unrelated"},
				Networks:            []string{"live-net", "self-net", "user-net"},
				Files:               []string{selfID + ".lock"},
				Warnings:            1,
				EngineCleanedUp:     true,
				EngineCleanupFailed: true,
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			docker := tt.docker
			docker.Containers = slices.Clone(docker.Containers)
			docker.Networks = slices.Clone(docker.Networks)

			advisories := &tu.AdvisoryRecorder{}
			r := newRun(dir, selfID, &docker, advisories)

			if tt.started {
				// The engine calls Start for every new batch of functions.
				start(t, r)
				start(t, r)
			}

			got := want{}
			release := func() {
				got.EngineCleanedUp = true

				if !tt.engineRemovesNetwork {
					return
				}

				if _, err := docker.NetworkRemove(t.Context(), "self-net", client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
					got.EngineCleanupFailed = true
				}
			}

			// The processor's Cleanup runs before rendering and again on exit.
			r.Close(t.Context(), release)
			r.Close(t.Context(), release)

			got.Containers = docker.ContainerIDs()
			got.Networks = docker.NetworkNames()
			got.Files = dirEntries(t, dir)
			got.Warnings = len(advisories.Advisories())

			if diff := gcmp.Diff(tt.want, got, unordered...); diff != "" {
				t.Errorf("\n%s\nClose(...): -want, +got:\n%s", tt.reason, diff)
			}
		})
	}
}

// TestRunReclaimsARunThatDied pins reaping end to end, with the run IDs New generates and the leases runs
// really take: a later run leaves a run's resources alone while that run is alive, and removes them once
// its process is gone.
func TestRunReclaimsARunThatDied(t *testing.T) {
	type left struct {
		Containers []string
		Networks   []string
	}

	dir := t.TempDir()
	docker := &tu.FakeDocker{}
	newRun := func() *Run {
		return New(dir, func() (Docker, error) { return docker, nil }, logging.NewNopLogger(), &tu.AdvisoryRecorder{})
	}

	first := newRun()
	start(t, first)

	docker.Containers = []container.Summary{tu.DockerContainer("fn", running, tu.DockerSession(first.ID()), "net")}
	docker.Networks = []network.Summary{tu.DockerNetwork("net", tu.DockerSession(first.ID()))}

	start(t, newRun())

	got := map[string]left{"WhileAlive": {Containers: docker.ContainerIDs(), Networks: docker.NetworkNames()}}

	// The first run's process dies: the kernel drops its lock, and its lease file stays.
	if err := first.lease.Close(); err != nil {
		t.Fatal(err)
	}

	start(t, newRun())

	got["AfterItDied"] = left{Containers: docker.ContainerIDs(), Networks: docker.NetworkNames()}

	want := map[string]left{
		"WhileAlive":  {Containers: []string{"fn"}, Networks: []string{"net"}},
		"AfterItDied": {},
	}
	if diff := gcmp.Diff(want, got, unordered...); diff != "" {
		t.Errorf("what later runs left of a run, -want, +got:\n%s", diff)
	}
}
