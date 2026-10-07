// Package envtestreaper kills envtest servers that earlier test runs left behind.
//
// envtest runs kube-apiserver and etcd as children of the test binary, and only a deferred Environment.Stop ends
// them. When the binary dies without running its defers (the -timeout panic, a kill, a Ctrl+C), they are reparented to
// PID 1 and run until someone kills them, piling up across runs (#524). Calling Reap from TestMain kills them when the
// next run starts.
package envtestreaper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// isServer reports whether name is that of a long-running server envtest starts.
func isServer(name string) bool {
	return name == "kube-apiserver" || name == "etcd"
}

// candidate is an orphaned process named like an envtest server.
type candidate struct {
	proc *process.Process
	// exe is the absolute path of the process's executable.
	exe string
}

// Reap SIGKILLs every envtest server orphaned by an earlier run, and reports through logf what it killed and what it
// could not. A process whose parent is PID 1 has lost the test binary that started it, so it cannot belong to a live
// run. Reap is best-effort: it never fails, and it skips any process it cannot inspect.
func Reap(logf func(format string, args ...any)) {
	pids, err := process.Pids()
	if err != nil {
		logf("envtest reaper: cannot list processes: %v", err)
		return
	}

	// Look up the executable only of orphans named like an envtest server.
	var candidates []candidate

	for _, pid := range pids {
		// Not process.NewProcess, which first checks the PID exists and fetches its creation time: most of the
		// scan's cost, for nothing Kill uses. A PID that has exited meanwhile fails Ppid and is skipped.
		p := &process.Process{Pid: pid}

		if ppid, err := p.Ppid(); err != nil || ppid != 1 {
			continue
		}

		if name, err := p.Name(); err != nil || !isServer(name) {
			continue
		}

		if exe, err := p.Exe(); err == nil {
			candidates = append(candidates, candidate{proc: p, exe: exe})
		}
	}

	var killed []string

	for _, c := range selectServers(candidates, binaryDirs()) {
		if err := c.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			logf("envtest reaper: cannot kill %s (pid %d): %v", c.exe, c.proc.Pid, err)
			continue
		}

		killed = append(killed, fmt.Sprintf("%s (pid %d)", filepath.Base(c.exe), c.proc.Pid))
	}

	if len(killed) > 0 {
		logf("envtest reaper: killed %d envtest server(s) orphaned by an earlier run: %s",
			len(killed), strings.Join(killed, ", "))
	}
}

// selectServers returns the candidates whose executable is an envtest server directly in one of dirs.
func selectServers(candidates []candidate, dirs []string) []candidate {
	var selected []candidate

	for _, c := range candidates {
		exe := filepath.Clean(c.exe)
		if filepath.IsAbs(exe) && isServer(filepath.Base(exe)) && slices.Contains(dirs, filepath.Dir(exe)) {
			selected = append(selected, c)
		}
	}

	return selected
}

// binaryDirs returns every directory a test run could have started envtest's servers from: KUBEBUILDER_ASSETS, each
// version in setup-envtest's store, and envtest's built-in default (where the Earthfile installs them). All are
// included, not only the one envtest would use now, because an orphan may come from a run configured differently.
// Each is also included with its symlinks resolved, as the process table may report the resolved path.
func binaryDirs() []string {
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
