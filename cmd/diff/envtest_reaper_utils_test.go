package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtest runs kube-apiserver and etcd as children of the test binary, and only a deferred Environment.Stop ends
// them. When the binary dies without running its defers (the -timeout panic, a kill, a Ctrl+C), they are reparented to
// PID 1 and run until someone kills them, piling up across runs (#524). reapOrphanedEnvtestServers, called from
// TestMain, kills them when the next run starts.

// envtestServers are the names of the long-running servers envtest starts.
var envtestServers = []string{"kube-apiserver", "etcd"}

// process is the part of a process-table entry the reaper needs.
type process struct {
	PID  int
	PPID int
	// Exe is the absolute path of the process's executable.
	Exe string
}

// isOrphanCandidate reports whether a process with parent ppid and command name might be an orphaned envtest server.
// The platform listers use it to skip looking up the executable of every other process.
func isOrphanCandidate(ppid int, name string) bool {
	return ppid == 1 && slices.Contains(envtestServers, name)
}

// reapOrphanedEnvtestServers SIGKILLs every envtest server orphaned by an earlier run. A process whose parent is PID 1
// has lost the test binary that started it, so it cannot belong to a live run. It is best-effort: failures are logged
// and never fail the suite.
func reapOrphanedEnvtestServers() {
	procs, err := listOrphanCandidates()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest reaper: cannot list processes: %v\n", err)
		return
	}

	var killed []string

	for _, p := range selectOrphanedEnvtestServers(procs, envtestBinaryDirs()) {
		if err := kill(p.PID); err != nil {
			fmt.Fprintf(os.Stderr, "envtest reaper: cannot kill %s (pid %d): %v\n", p.Exe, p.PID, err)
			continue
		}

		killed = append(killed, fmt.Sprintf("%s (pid %d)", filepath.Base(p.Exe), p.PID))
	}

	if len(killed) > 0 {
		fmt.Fprintf(os.Stderr, "envtest reaper: killed %d envtest server(s) orphaned by an earlier run: %s\n",
			len(killed), strings.Join(killed, ", "))
	}
}

// kill SIGKILLs the process pid. A process that has already exited is not an error.
func kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()

	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	return nil
}

// selectOrphanedEnvtestServers returns the processes in procs whose parent is PID 1 and whose executable is an
// envtest server in one of dirs.
func selectOrphanedEnvtestServers(procs []process, dirs []string) []process {
	var selected []process

	for _, p := range procs {
		exe := filepath.Clean(p.Exe)
		if p.PPID == 1 && filepath.IsAbs(exe) && slices.Contains(envtestServers, filepath.Base(exe)) &&
			slices.Contains(dirs, filepath.Dir(exe)) {
			selected = append(selected, p)
		}
	}

	return selected
}

// envtestBinaryDirs returns every directory a run of the integration tests could have started envtest's servers
// from: KUBEBUILDER_ASSETS, each version in setup-envtest's store, and envtest's built-in default (where the Earthfile
// installs them). All are included, not only the one envtest would use now, because an orphan may come from a run
// configured differently. Each is also included with its symlinks resolved, as /proc reports the resolved path.
func envtestBinaryDirs() []string {
	dirs := []string{"/usr/local/kubebuilder/bin"}

	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		dirs = append(dirs, filepath.Clean(v))
	}

	if store, err := envtest.SetupEnvtestDefaultBinaryAssetsDirectory(); err == nil {
		versions, _ := os.ReadDir(store) // A missing store has no versions.
		for _, v := range versions {
			dirs = append(dirs, filepath.Join(store, v.Name()))
		}
	}

	for _, d := range dirs {
		if r, err := filepath.EvalSymlinks(d); err == nil && r != d {
			dirs = append(dirs, r)
		}
	}

	return dirs
}
