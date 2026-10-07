package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// listProcesses returns the processes for which keep, given a process's parent PID and command name, returns true.
// It reads /proc/<pid>/stat for every process and resolves the executable only of the processes it keeps. Processes
// it cannot inspect, such as one that exited meanwhile or another user's, are left out.
func listProcesses(keep func(ppid int, name string) bool) ([]process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	var procs []process

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // Not a process directory.
		}

		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}

		name, ppid, ok := parseProcStat(stat)
		if !ok || !keep(ppid, name) {
			continue
		}

		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			continue
		}

		// The link carries this suffix if the binary was removed or replaced after the process started.
		procs = append(procs, process{PID: pid, PPID: ppid, Exe: strings.TrimSuffix(exe, " (deleted)")})
	}

	return procs, nil
}

// parseProcStat returns the command name and parent PID from the contents of /proc/<pid>/stat, which reads
// "pid (comm) state ppid ...". The command name, cut to 15 bytes, may itself contain spaces and parentheses, so it
// runs to the last ')'.
func parseProcStat(stat []byte) (name string, ppid int, ok bool) {
	open, closing := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || closing < open {
		return "", 0, false
	}

	fields := strings.Fields(string(stat[closing+1:]))
	if len(fields) < 2 {
		return "", 0, false
	}

	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}

	return string(stat[open+1 : closing]), ppid, true
}

func TestParseProcStat(t *testing.T) {
	tests := map[string]struct {
		stat     string
		wantName string
		wantPPID int
		wantOK   bool
	}{
		"Apiserver":                {stat: "4711 (kube-apiserver) S 1 4711 4711 0 -1 ...", wantName: "kube-apiserver", wantPPID: 1, wantOK: true},
		"NameWithSpacesAndParens":  {stat: "12 (a (b) c) R 34 12 12 0", wantName: "a (b) c", wantPPID: 34, wantOK: true},
		"NoParens":                 {stat: "12 etcd S 1", wantOK: false},
		"TruncatedAfterName":       {stat: "12 (etcd) S", wantOK: false},
		"NonNumericParent":         {stat: "12 (etcd) S x", wantOK: false},
		"ClosingBeforeOpeningOnly": {stat: ") 12 (", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			gotName, gotPPID, gotOK := parseProcStat([]byte(tt.stat))
			if gotName != tt.wantName || gotPPID != tt.wantPPID || gotOK != tt.wantOK {
				t.Errorf("parseProcStat(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tt.stat, gotName, gotPPID, gotOK, tt.wantName, tt.wantPPID, tt.wantOK)
			}
		})
	}
}
