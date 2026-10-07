package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

// listProcesses returns the processes for which keep, given a process's parent PID and command name, returns true.
// It reads the whole process table with one sysctl and looks up the executable only of the processes it keeps.
// Processes it cannot inspect, such as one that exited meanwhile or another user's, are left out.
func listProcesses(keep func(ppid int, name string) bool) ([]process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}

	var procs []process

	for i := range kps {
		pid, ppid := int(kps[i].Proc.P_pid), int(kps[i].Eproc.Ppid)
		// p_comm is the executable's base name, cut to 16 bytes; envtest's server names fit.
		if !keep(ppid, unix.ByteSliceToString(kps[i].Proc.P_comm[:])) {
			continue
		}

		exe, err := execPath(pid)
		if err != nil {
			continue
		}

		procs = append(procs, process{PID: pid, PPID: ppid, Exe: exe})
	}

	return procs, nil
}

// execPath returns the path that the process pid was executed from. kern.procargs2 holds a 32-bit argc followed by
// that path, NUL-terminated, then the arguments and environment.
func execPath(pid int) (string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}

	if len(buf) < 4 {
		return "", errors.New("kern.procargs2 is too short to hold an executable path")
	}

	return unix.ByteSliceToString(buf[4:]), nil
}
