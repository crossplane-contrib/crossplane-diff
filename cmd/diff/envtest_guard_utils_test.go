package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Guards against leaking envtest's control plane (#524).
//
// envtest runs kube-apiserver and etcd as children of the test binary, each in its own process group, and only
// Environment.Stop ends them. The integration tests call Stop from a defer, which does not run when the binary dies
// abnormally: go test's -timeout fires a panic on its own goroutine, a kill ends the process outright, and a Ctrl+C
// reaches only the terminal's foreground process group, which envtest's children have left. Their parent is gone, so
// they are reparented to PID 1 and run until someone kills them.
//
// startEnvtestGuards installs three guards, all from TestMain, so no test pays for them:
//   - a reaper that, when the suite starts, kills envtest servers orphaned by an earlier run (covers SIGKILL, OOM and
//     job cancellation after the fact);
//   - a timer that kills this binary's own envtest servers just before the -test.timeout panic;
//   - a handler that kills them when the binary is interrupted or terminated, then re-raises the signal.
//
// The guards select processes by parent PID and executable path alone, so they never touch a server that belongs to a
// live run (its parent is that run's test binary, not PID 1) or one that envtest did not start.

// envtestServerBinaries are the long-running servers envtest starts, keyed to the environment variable envtest reads to
// override that one binary's location.
var envtestServerBinaries = map[string]string{
	"kube-apiserver": "TEST_ASSET_KUBE_APISERVER",
	"etcd":           "TEST_ASSET_ETCD",
}

const (
	// envtestAssetsEnv is the environment variable that envtest reads for the directory holding its binaries.
	envtestAssetsEnv = "KUBEBUILDER_ASSETS"

	// envtestDefaultAssetsDir is where envtest looks for its binaries when nothing else says where they are. The
	// Earthfile's go-test target installs them there.
	envtestDefaultAssetsDir = "/usr/local/kubebuilder/bin"

	// envtestDeadlineMargin is how long before the -test.timeout panic the deadline guard kills this binary's envtest
	// servers. Killing them takes milliseconds; the margin absorbs timer lateness on a loaded machine.
	envtestDeadlineMargin = 5 * time.Second
)

// process is the part of a process-table entry the guards need.
type process struct {
	PID  int
	PPID int
	// Exe is the absolute path of the process's executable, or empty if it could not be determined.
	Exe string
}

// startEnvtestGuards reaps envtest servers orphaned by earlier runs, then arms the deadline and signal guards for this
// run. timeout is the value of -test.timeout. The returned function disarms the deadline and signal guards.
//
// Every guard is best-effort: a failure is logged and never fails the suite.
func startEnvtestGuards(timeout time.Duration) (stop func()) {
	killEnvtestServers(1, "reaped servers orphaned by an earlier run")

	var deadline *time.Timer
	if delay, ok := envtestDeadlineGuardDelay(timeout); ok {
		deadline = time.AfterFunc(delay, func() {
			killEnvtestServers(os.Getpid(), fmt.Sprintf("-test.timeout=%s expires in %s", timeout, timeout-delay))
		})
	}

	// Notifying on a signal the binary was started with ignored (nohup, for one) would stop it being ignored.
	var sigs []os.Signal
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}

	received := make(chan os.Signal, 1)
	disarmed := make(chan struct{})

	signal.Notify(received, sigs...)

	go func() {
		select {
		case sig := <-received:
			killEnvtestServers(os.Getpid(), "received "+sig.String())
			// Restore the default disposition and re-raise, so the binary still dies of the signal it was sent.
			signal.Reset(sigs...)

			if err := reraise(sig); err != nil {
				os.Exit(1)
			}
		case <-disarmed:
		}
	}()

	return func() {
		if deadline != nil {
			deadline.Stop()
		}

		signal.Stop(received)
		close(disarmed)
	}
}

// envtestDeadlineGuardDelay returns how long after the start of the run the deadline guard should fire, given the
// value of -test.timeout, and false if there is no timeout to guard. It fires envtestDeadlineMargin before the
// timeout, or a tenth of the timeout before it if that is sooner, so a short -timeout does not lose most of its run.
func envtestDeadlineGuardDelay(timeout time.Duration) (time.Duration, bool) {
	if timeout <= 0 {
		return 0, false
	}

	return timeout - min(envtestDeadlineMargin, timeout/10), true
}

// killEnvtestServers SIGKILLs every envtest server whose parent is parentPID and logs what it killed, prefixed by
// reason. SIGKILL rather than SIGTERM, because a server that outlives this binary must not be left to finish a
// graceful shutdown on its own.
func killEnvtestServers(parentPID int, reason string) {
	procs, err := listProcesses(func(ppid int, name string) bool {
		_, server := envtestServerBinaries[name]
		return ppid == parentPID && server
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest guard: cannot list processes: %v\n", err)
		return
	}

	var killed []string

	for _, p := range selectEnvtestProcesses(procs, parentPID, currentEnvtestBinaryPaths()) {
		if err := kill(p.PID); err != nil {
			fmt.Fprintf(os.Stderr, "envtest guard: cannot kill %s (pid %d): %v\n", p.Exe, p.PID, err)
			continue
		}

		killed = append(killed, fmt.Sprintf("%s (pid %d)", filepath.Base(p.Exe), p.PID))
	}

	if len(killed) > 0 {
		fmt.Fprintf(os.Stderr, "envtest guard: %s: killed %d envtest server(s): %s\n",
			reason, len(killed), strings.Join(killed, ", "))
	}
}

// kill SIGKILLs the process pid. A process that has already exited is not an error.
func kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release() //nolint:errcheck // Releasing only frees the handle.

	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	return nil
}

// reraise sends sig to this process.
func reraise(sig os.Signal) error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}

	return p.Signal(sig)
}

// selectEnvtestProcesses returns the processes in procs whose parent is parentPID and whose executable is one of
// binaries, the envtest server paths from envtestBinaryPaths.
func selectEnvtestProcesses(procs []process, parentPID int, binaries map[string]bool) []process {
	var selected []process

	for _, p := range procs {
		if p.PPID == parentPID && filepath.IsAbs(p.Exe) && binaries[filepath.Clean(p.Exe)] {
			selected = append(selected, p)
		}
	}

	return selected
}

// currentEnvtestBinaryPaths returns envtestBinaryPaths for this process's environment and the versions in
// setup-envtest's store, each also with its symlinks resolved, since a process table may report either form.
func currentEnvtestBinaryPaths() map[string]bool {
	store, err := envtest.SetupEnvtestDefaultBinaryAssetsDirectory()
	if err != nil {
		store = ""
	}

	var versions []string

	if store != "" {
		entries, _ := os.ReadDir(store) // A missing store has no versions.
		for _, e := range entries {
			if e.IsDir() {
				versions = append(versions, e.Name())
			}
		}
	}

	paths := envtestBinaryPaths(os.LookupEnv, store, versions)

	var resolved []string

	for p := range paths {
		if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
			resolved = append(resolved, r)
		}
	}

	for _, r := range resolved {
		paths[r] = true
	}

	return paths
}

// envtestBinaryPaths returns every path at which a run of the integration tests could have started an envtest
// server: wherever lookupEnv says envtest's binaries are (KUBEBUILDER_ASSETS and the per-binary TEST_ASSET_*
// overrides), envtest's built-in default directory, and each version directory in setup-envtest's store (store,
// whose subdirectories are storeVersions). Every location is included, not just the one envtest would pick now,
// because an orphan may come from a run configured differently.
func envtestBinaryPaths(lookupEnv func(string) (string, bool), store string, storeVersions []string) map[string]bool {
	dirs := []string{envtestDefaultAssetsDir}

	if v, ok := lookupEnv(envtestAssetsEnv); ok && v != "" {
		dirs = append(dirs, v)
	}

	if store != "" {
		for _, v := range storeVersions {
			dirs = append(dirs, filepath.Join(store, v))
		}
	}

	paths := make(map[string]bool)

	for name, overrideEnv := range envtestServerBinaries {
		for _, d := range dirs {
			paths[filepath.Join(d, name)] = true
		}

		if v, ok := lookupEnv(overrideEnv); ok && v != "" {
			paths[filepath.Clean(v)] = true
		}
	}

	return paths
}
