package envtestreaper

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestSelectServers(t *testing.T) {
	const store = "/Users/u/Library/Application Support/io.kubebuilder.envtest/k8s/1.32.0-darwin-arm64"

	dirs := []string{store, "/usr/local/kubebuilder/bin"}

	// Reap passes only PPID-1 processes named like a server; the executable path decides the rest.
	candidates := []candidate{
		{exe: store + "/kube-apiserver"},              // orphaned envtest apiserver
		{exe: store + "/etcd"},                        // orphaned envtest etcd
		{exe: "/usr/local/kubebuilder/bin/etcd"},      // orphaned envtest etcd (Earthly layout)
		{exe: store + "/../1.32.0-darwin-arm64/etcd"}, // unclean path to an envtest binary
		{exe: "/opt/homebrew/bin/etcd"},               // a real etcd daemon
		{exe: "/usr/local/bin/kube-apiserver"},        // a real apiserver
		{exe: store + "/kubectl"},                     // a non-server envtest binary
		{exe: "/usr/local/kubebuilder/bin-x/etcd"},    // a sibling dir sharing a prefix
		{exe: store + "/sub/etcd"},                    // nested below an envtest dir
		{exe: "etcd"},                                 // a bare name, not a path
		{exe: ""},                                     // unknown executable
	}

	selected := selectServers(candidates, dirs)

	got := make([]string, 0, len(selected))
	for _, c := range selected {
		got = append(got, c.exe)
	}

	want := []string{
		store + "/kube-apiserver",
		store + "/etcd",
		"/usr/local/kubebuilder/bin/etcd",
		store + "/../1.32.0-darwin-arm64/etcd",
	}

	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("selectServers() -want +got:\n%s", diff)
	}
}
