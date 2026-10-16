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

// Package dockerrun ties the Docker resources a crossplane-diff run creates — function containers, the
// render container and the render network — to that run, and reclaims the ones left behind by runs that
// died without cleaning up (SIGKILL, OOM, a second Ctrl+C, a CI runner tearing down the job).
//
// Ownership is carried by labels. A run's ID is the session ID it hands the upstream render engine
// (render.EngineFlags.SessionID), which labels every container and network the engine creates
// render.crossplane.io/session=<run ID>, alongside render.crossplane.io/managed-by=crossplane-cli. A
// run's resources are exactly the ones carrying both labels with its ID, so a container or network that
// lacks either, such as one created before session labels existed, is never touched.
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
	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/renderer/types"
	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	"github.com/moby/moby/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

const (
	// runIDLength is the length of a run ID: 16 lowercase hex characters (64 random bits), so a session
	// label carrying one cannot belong to anything but that run.
	runIDLength = 16

	leaseExt = ".lock"

	// reapTimeout bounds reclaiming dead runs' resources, so a slow Docker daemon cannot hold up a run.
	reapTimeout = 10 * time.Second
)

// Docker is the subset of the Docker API client a Run uses.
type Docker interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	NetworkList(ctx context.Context, options client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	Close() error
}

// NewDockerClient returns a Docker API client configured from the environment, the same way the
// upstream render engine configures its own.
func NewDockerClient() (Docker, error) {
	return client.New(client.FromEnv)
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
	id        string
	dir       string
	newDocker func() (Docker, error)
	log       logging.Logger
	// advisories is where a failure to clean up is reported, since it leaves Docker resources behind.
	advisories dt.Advisories

	// docker is set while the run is started.
	docker Docker
	// lease is the open, locked lease file while the run is started; nil when leasing failed.
	lease *os.File
}

// New returns a Run that keeps its lease in dir ("" disables leasing and reaping) and talks to Docker
// through a client from newDocker. New does no I/O.
func New(dir string, newDocker func() (Docker, error), log logging.Logger, advisories dt.Advisories) *Run {
	b := make([]byte, runIDLength/2)
	_, _ = rand.Read(b) // Never returns an error.

	return &Run{id: hex.EncodeToString(b), dir: dir, newDocker: newDocker, log: log, advisories: advisories}
}

// ID is the run's ID. It must be the render engine's session ID: the session label is what makes a
// container or network this run's.
func (r *Run) ID() string {
	return r.id
}

// Start prepares the run to create Docker resources; call it before starting any. It takes the run's
// lease and reclaims what dead runs left behind. Both are best effort: a failure there is at most a
// warning. Only failing to create a Docker client is an error. Start is a no-op on a started run.
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

	return nil
}

// Close removes every container this run created, calls release, then removes every network of the
// run still left and gives up its lease. release is the render engine's own cleanup: it removes the
// network the engine created, which it can only do once no container is attached, so it runs between
// the two. Close removes what upstream cleanup should already have removed too, so a container or
// network upstream leaves behind does not outlive the run. If anything cannot be removed the lease file
// is kept, so the next run reclaims it, and a warning is raised. On a run that is not started, Close
// only calls release.
func (r *Run) Close(ctx context.Context, release func()) {
	if r.docker == nil {
		release()
		return
	}

	err := r.removeContainers(ctx, r.id)

	release()

	if err == nil {
		err = r.removeNetworks(ctx, r.id)
	}

	if err != nil {
		r.advisories.Raise(dt.OutputWarning{
			Message: "Some Docker containers or networks this run created could not be cleaned up; the next crossplane-diff run will retry",
			Context: map[string]string{"error": err.Error()},
		})
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

		err := r.removeContainers(ctx, id)
		if err == nil {
			// A container still attached would make removing its network fail, so networks wait for
			// every container to be gone.
			err = r.removeNetworks(ctx, id)
		}

		if err != nil {
			errs = append(errs, errors.Wrapf(err, "run %s", id))
		} else {
			r.log.Debug("Reclaimed Docker resources left by a dead run", "run", id)
		}

		releaseLease(f, p, err == nil)
	}

	if len(errs) > 0 {
		r.advisories.Raise(dt.OutputWarning{
			Message: "Cannot clean up Docker containers or networks left behind by an earlier crossplane-diff run; the next run will retry",
			Context: map[string]string{"error": errors.Join(errs...).Error()},
		})
	}
}

// removeContainers removes every container of run id, running or not.
func (r *Run) removeContainers(ctx context.Context, id string) error {
	res, err := r.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: sessionFilter(id)})
	if err != nil {
		return errors.Wrap(err, "cannot list Docker containers")
	}

	var errs []error

	for _, c := range res.Items {
		// Force stops the container if it is still running.
		if _, err := r.docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, errors.Wrapf(err, "cannot remove container %s", c.ID))
		}
	}

	return errors.Join(errs...)
}

// removeNetworks removes every network of run id. A run that used the local render engine, or a
// network the user supplied, has none.
func (r *Run) removeNetworks(ctx context.Context, id string) error {
	res, err := r.docker.NetworkList(ctx, client.NetworkListOptions{Filters: sessionFilter(id)})
	if err != nil {
		return errors.Wrap(err, "cannot list Docker networks")
	}

	var errs []error

	for _, n := range res.Items {
		if _, err := r.docker.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, errors.Wrapf(err, "cannot remove network %s", n.Name))
		}
	}

	return errors.Join(errs...)
}

// sessionFilter matches what the upstream render engine created for run id. Docker requires every label
// filter to match, so a resource is matched only if it carries both labels.
func sessionFilter(id string) client.Filters {
	return make(client.Filters).Add("label",
		render.LabelKeySession+"="+id,
		render.LabelKeyManagedBy+"="+render.LabelValueManagedByCrossplane)
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
