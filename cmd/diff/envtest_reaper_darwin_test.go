package main

import (
	"golang.org/x/sys/unix"
)

// listOrphanCandidates returns the processes for which isOrphanCandidate holds, read from the kern.proc.all sysctl.
// Processes it cannot inspect, such as one that exited meanwhile, are left out.
func listOrphanCandidates() ([]process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}

	var procs []process

	for i := range kps {
		pid, ppid := int(kps[i].Proc.P_pid), int(kps[i].Eproc.Ppid)
		// p_comm is the executable's base name, cut to 16 bytes; envtest's server names fit.
		if !isOrphanCandidate(ppid, unix.ByteSliceToString(kps[i].Proc.P_comm[:])) {
			continue
		}

		// kern.procargs2 holds a 32-bit argc, then the NUL-terminated path the process was executed from.
		args, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil || len(args) < 4 {
			continue
		}

		procs = append(procs, process{PID: pid, PPID: ppid, Exe: unix.ByteSliceToString(args[4:])})
	}

	return procs, nil
}
