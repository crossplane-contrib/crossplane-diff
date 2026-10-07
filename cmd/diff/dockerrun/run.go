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

// Package dockerrun ties the Docker resources a crossplane-diff run creates — function containers and
// the render network — to that run, and reclaims the ones left behind by runs that died without
// cleaning up (SIGKILL, OOM, a second Ctrl+C, a CI runner tearing down the job).
//
// Ownership is carried by names: the upstream Docker runtime cannot label a function container, but it
// does let the caller name it, so every function container a run starts ends in "-diff-<run ID>", and
// the render network the run creates is "crossplane-diff-<run ID>" (also labelled). The run's render
// containers are unnamed, so they are recognised by being on that network.
//
// Liveness is a lease: a run holds an exclusive lock on "<run ID>.lock" in the user's cache directory
// for as long as it has resources. The operating system drops the lock when the process exits however
// it exits, so a lease file whose lock can be taken belongs to a dead run. That needs no process table,
// cannot be fooled by PID reuse, and keeps concurrent runs — other worktrees, other test processes —
// from touching each other's resources. A lease that is not in this user's cache directory (another
// user, another container sharing the Docker socket) is never seen, so its resources are never touched.
package dockerrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

const (
	// LabelRunID labels the render network a run creates with the ID of that run.
	LabelRunID = "crossplane-diff.io/run-id"

	// runIDLength is the length of a run ID: 16 lowercase hex characters (64 random bits), so a name
	// ending in one cannot belong to anything but that run.
	runIDLength = 16

	leaseExt = ".lock"

	// reapTimeout bounds reclaiming dead runs' resources, so a slow Docker daemon cannot hold up a run.
	reapTimeout = 10 * time.Second
)

// Docker is the subset of the Docker API client a Run uses.
type Docker interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
	Close() error
}

// NewDockerClient returns a Docker API client configured from the environment, the same way the
// upstream render engine configures its own.
func NewDockerClient() (Docker, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// DefaultLeaseDir is where runs keep their leases: a per-user directory, so one user's runs never judge
// another's. It returns "" when the user has no cache directory, which disables leasing and reaping.
func DefaultLeaseDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}

	return filepath.Join(dir, "crossplane-diff", "runs")
}

// Run is one crossplane-diff run's claim on the Docker resources it creates. It is not safe for
// concurrent use; the render engine that owns it serializes access.
type Run struct {
	id          string
	dir         string
	newDocker   func() (Docker, error)
	ownsNetwork bool
	log         logging.Logger

	// docker is set while the run is started.
	docker Docker
	// lease is the open, locked lease file while the run is started; nil when leasing failed.
	lease *os.File
}

// New returns a Run that keeps its lease in dir ("" disables leasing and reaping) and talks to Docker
// through a client from newDocker. ownsNetwork says whether the run creates the render network itself,
// which it does unless the user supplied one or the render engine needs none. New does no I/O.
func New(dir string, newDocker func() (Docker, error), ownsNetwork bool, log logging.Logger) *Run {
	b := make([]byte, runIDLength/2)
	_, _ = rand.Read(b) // Never returns an error.

	return &Run{id: hex.EncodeToString(b), dir: dir, newDocker: newDocker, ownsNetwork: ownsNetwork, log: log}
}

// NetworkName is the name of the render network this run creates when it owns one.
func (r *Run) NetworkName() string {
	return networkName(r.id)
}

// ContainerName is the name of the container this run starts for a function package.
func (r *Run) ContainerName(pkg string) string {
	return containerName(pkg, r.id)
}

// Start prepares the run to create Docker resources; call it before starting any. It takes the run's
// lease, reclaims what dead runs left behind, and creates the run's render network if it owns one.
// Leasing and reaping are best effort: a failure there is at most a warning. Only failing to create
// the render network is an error, since rendering needs it. Start is a no-op on a started run.
func (r *Run) Start(ctx context.Context) error {
	if r.docker != nil {
		return nil
	}

	docker, err := r.newDocker()
	if err != nil {
		return errors.Wrap(err, "cannot create Docker client")
	}

	r.docker = docker

	if r.dir != "" {
		r.lease, err = acquireLease(r.dir, r.id)
		if err != nil {
			// Unleased resources are invisible to reapers, which is safe: they are only at risk of
			// leaking, as they always were.
			r.log.Debug("Cannot take a lease on this run's Docker resources; they will not be reclaimed if it dies",
				"dir", r.dir, "error", err)
		}

		r.reapDead(ctx)
	}

	if !r.ownsNetwork {
		return nil
	}

	name := r.NetworkName()
	if _, err := docker.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{LabelRunID: r.id},
	}); err != nil {
		return errors.Wrapf(err, "cannot create Docker network %q", name)
	}

	return nil
}

// Close removes every container and the network this run created, then gives up its lease. It
// removes what upstream cleanup should already have removed too, so a container upstream leaves behind
// does not outlive the run. If anything cannot be removed the lease file is kept, so the next run
// reclaims it, and a warning is raised. Close is a no-op on a run that is not started.
func (r *Run) Close(ctx context.Context) {
	if r.docker == nil {
		return
	}

	err := r.reap(ctx, r.id)
	if err != nil {
		r.log.Info("Some function containers could not be cleaned up; the next crossplane-diff run will retry",
			"error", err)
	}

	releaseLease(r.lease, leasePath(r.dir, r.id), err == nil)

	if cerr := r.docker.Close(); cerr != nil {
		r.log.Debug("Error closing Docker client", "error", cerr)
	}

	r.docker, r.lease = nil, nil
}

// reapDead reclaims the resources of every run whose lease is in r.dir but held by no process.
func (r *Run) reapDead(ctx context.Context) {
	paths, err := filepath.Glob(filepath.Join(r.dir, "*"+leaseExt))
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, reapTimeout)
	defer cancel()

	var errs []error

	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), leaseExt)
		if id == r.id || !validRunID(id) {
			continue
		}

		f, ok := tryLease(p)
		if !ok {
			continue
		}

		err := r.reap(ctx, id)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "run %s", id))
		} else {
			r.log.Debug("Reclaimed Docker resources left by a dead run", "run", id)
		}

		releaseLease(f, p, err == nil)
	}

	if len(errs) > 0 {
		r.log.Info("Cannot clean up function containers left behind by an earlier crossplane-diff run; the next run will retry",
			"error", errors.Join(errs...))
	}
}

// reap removes run id's containers, then its network.
func (r *Run) reap(ctx context.Context, id string) error {
	containers, err := r.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return errors.Wrap(err, "cannot list Docker containers")
	}

	var errs []error

	for _, c := range containers {
		if !ownedBy(c, id) {
			continue
		}

		// Force stops the container if it is still running.
		if err := r.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, errors.Wrapf(err, "cannot remove container %s", c.ID))
		}
	}

	if len(errs) > 0 {
		// A container still attached would make removing the network fail anyway.
		return errors.Join(errs...)
	}

	// A run that used the local render engine, or a network the user supplied, created no network.
	if err := r.docker.NetworkRemove(ctx, networkName(id)); err != nil && !cerrdefs.IsNotFound(err) {
		return errors.Wrapf(err, "cannot remove network %s", networkName(id))
	}

	return nil
}

// ownedBy reports whether container c belongs to run id: it carries the run's name suffix, or it is
// attached to the run's network (the run's unnamed render containers).
func ownedBy(c container.Summary, id string) bool {
	for _, n := range c.Names {
		if strings.HasSuffix(strings.TrimPrefix(n, "/"), containerSuffix(id)) {
			return true
		}
	}

	if c.NetworkSettings == nil {
		return false
	}

	_, ok := c.NetworkSettings.Networks[networkName(id)]

	return ok
}

func networkName(id string) string {
	return "crossplane-diff-" + id
}

func validRunID(id string) bool {
	if len(id) != runIDLength {
		return false
	}

	_, err := hex.DecodeString(id)

	return err == nil && strings.ToLower(id) == id
}

func leasePath(dir, id string) string {
	return filepath.Join(dir, id+leaseExt)
}

// acquireLease creates and locks run id's lease file. The file is locked under a temporary name and
// renamed into place, so a reaper never sees a lease file that is not yet locked.
func acquireLease(dir, id string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.Wrap(err, "cannot create lease directory")
	}

	f, err := os.CreateTemp(dir, id+"-*.tmp")
	if err != nil {
		return nil, errors.Wrap(err, "cannot create lease file")
	}

	if err := lock(f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())

		return nil, errors.Wrap(err, "cannot lock lease file")
	}

	if err := os.Rename(f.Name(), leasePath(dir, id)); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())

		return nil, errors.Wrap(err, "cannot publish lease file")
	}

	return f, nil
}

// tryLease takes the lease at path if no process holds it, i.e. if its run is dead. If another reaper
// reclaimed the run between our open and our lock, reaping it again finds nothing left to remove.
func tryLease(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // The path is a lease file in our own lease directory.
	if err != nil {
		return nil, false
	}

	if err := lock(f); err != nil {
		_ = f.Close()
		return nil, false
	}

	return f, true
}

// releaseLease gives up a lease, removing its file first when the run's resources are all gone, so no
// one can take a lease that no longer protects anything. A nil lease (leasing failed) is a no-op.
func releaseLease(f *os.File, path string, remove bool) {
	if f == nil {
		return
	}

	if remove {
		_ = os.Remove(path)
	}

	_ = f.Close()
}
