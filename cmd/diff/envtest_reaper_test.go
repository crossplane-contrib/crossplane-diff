package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestSelectOrphanedEnvtestServers(t *testing.T) {
	const store = "/Users/u/Library/Application Support/io.kubebuilder.envtest/k8s/1.32.0-darwin-arm64"

	dirs := []string{store, "/usr/local/kubebuilder/bin"}

	procs := []process{
		{PID: 101, PPID: 1, Exe: store + "/kube-apiserver"},              // orphaned envtest apiserver
		{PID: 102, PPID: 1, Exe: store + "/etcd"},                        // orphaned envtest etcd
		{PID: 103, PPID: 1, Exe: "/usr/local/kubebuilder/bin/etcd"},      // orphaned envtest etcd (Earthly layout)
		{PID: 104, PPID: 1, Exe: store + "/../1.32.0-darwin-arm64/etcd"}, // unclean path to an envtest binary
		{PID: 201, PPID: 4242, Exe: store + "/kube-apiserver"},           // a live run's apiserver
		{PID: 401, PPID: 1, Exe: "/opt/homebrew/bin/etcd"},               // a real etcd daemon
		{PID: 402, PPID: 1, Exe: "/usr/local/bin/kube-apiserver"},        // a real apiserver
		{PID: 403, PPID: 1, Exe: store + "/kubectl"},                     // a non-server envtest binary
		{PID: 404, PPID: 1, Exe: "/usr/local/kubebuilder/bin-x/etcd"},    // a sibling dir sharing a prefix
		{PID: 405, PPID: 1, Exe: store + "/sub/etcd"},                    // nested below an envtest dir
		{PID: 406, PPID: 1, Exe: "etcd"},                                 // a bare name, not a path
		{PID: 407, PPID: 1, Exe: ""},                                     // unknown executable
	}

	selected := selectOrphanedEnvtestServers(procs, dirs)

	got := make([]int, 0, len(selected))
	for _, p := range selected {
		got = append(got, p.PID)
	}

	if diff := cmp.Diff([]int{101, 102, 103, 104}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("selectOrphanedEnvtestServers() PIDs -want +got:\n%s", diff)
	}
}
