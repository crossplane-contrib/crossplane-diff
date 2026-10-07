package main

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtestBinaryPaths(t *testing.T) {
	const store = "/home/u/.local/share/kubebuilder-envtest/k8s"

	tests := map[string]struct {
		env           map[string]string
		storeVersions []string
		want          []string
	}{
		"NothingConfiguredOnlyTheBuiltInDefault": {
			want: []string{
				"/usr/local/kubebuilder/bin/kube-apiserver",
				"/usr/local/kubebuilder/bin/etcd",
			},
		},
		"EveryVersionInTheSetupEnvtestStore": {
			storeVersions: []string{"1.30.3-linux-amd64", "1.32.0-linux-amd64"},
			want: []string{
				"/usr/local/kubebuilder/bin/kube-apiserver",
				"/usr/local/kubebuilder/bin/etcd",
				store + "/1.30.3-linux-amd64/kube-apiserver",
				store + "/1.30.3-linux-amd64/etcd",
				store + "/1.32.0-linux-amd64/kube-apiserver",
				store + "/1.32.0-linux-amd64/etcd",
			},
		},
		"KubebuilderAssetsAndPerBinaryOverrides": {
			env: map[string]string{
				"KUBEBUILDER_ASSETS":        "/opt/assets/",
				"TEST_ASSET_KUBE_APISERVER": "/opt/custom/kube-apiserver",
				"TEST_ASSET_ETCD":           "/opt/custom/etcd-3.5",
			},
			want: []string{
				"/usr/local/kubebuilder/bin/kube-apiserver",
				"/usr/local/kubebuilder/bin/etcd",
				"/opt/assets/kube-apiserver",
				"/opt/assets/etcd",
				"/opt/custom/kube-apiserver",
				"/opt/custom/etcd-3.5",
			},
		},
		"EmptyValuesAreIgnored": {
			env: map[string]string{"KUBEBUILDER_ASSETS": "", "TEST_ASSET_ETCD": ""},
			want: []string{
				"/usr/local/kubebuilder/bin/kube-apiserver",
				"/usr/local/kubebuilder/bin/etcd",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			lookup := func(k string) (string, bool) {
				v, ok := tt.env[k]
				return v, ok
			}

			got := envtestBinaryPaths(lookup, store, tt.storeVersions)

			want := make(map[string]bool, len(tt.want))
			for _, p := range tt.want {
				want[p] = true
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("envtestBinaryPaths() -want +got:\n%s", diff)
			}
		})
	}
}

func TestSelectEnvtestProcesses(t *testing.T) {
	const (
		store = "/Users/u/Library/Application Support/io.kubebuilder.envtest/k8s/1.32.0-darwin-arm64"
		self  = 4242
	)

	binaries := map[string]bool{
		store + "/kube-apiserver":                   true,
		store + "/etcd":                             true,
		"/usr/local/kubebuilder/bin/kube-apiserver": true,
		"/usr/local/kubebuilder/bin/etcd":           true,
	}

	procs := []process{
		{PID: 101, PPID: 1, Exe: store + "/kube-apiserver"},              // orphaned envtest apiserver
		{PID: 102, PPID: 1, Exe: store + "/etcd"},                        // orphaned envtest etcd
		{PID: 103, PPID: 1, Exe: "/usr/local/kubebuilder/bin/etcd"},      // orphaned envtest etcd (Earthly layout)
		{PID: 104, PPID: 1, Exe: store + "/../1.32.0-darwin-arm64/etcd"}, // unclean path to an envtest binary
		{PID: 201, PPID: self, Exe: store + "/kube-apiserver"},           // this run's live apiserver
		{PID: 202, PPID: self, Exe: store + "/etcd"},                     // this run's live etcd
		{PID: 301, PPID: 999, Exe: store + "/etcd"},                      // another live run's etcd
		{PID: 401, PPID: 1, Exe: "/opt/homebrew/bin/etcd"},               // a real etcd daemon
		{PID: 402, PPID: 1, Exe: "/usr/local/bin/kube-apiserver"},        // a real apiserver
		{PID: 403, PPID: 1, Exe: store + "/kubectl"},                     // a non-server envtest binary
		{PID: 404, PPID: 1, Exe: "/usr/local/kubebuilder/bin-x/etcd"},    // a sibling dir sharing a prefix
		{PID: 405, PPID: 1, Exe: "etcd"},                                 // a bare name, not a path
		{PID: 406, PPID: 1, Exe: ""},                                     // unknown executable
		{PID: 1, PPID: 0, Exe: "/sbin/launchd"},                          // init itself
	}

	tests := map[string]struct {
		parent int
		want   []int
	}{
		"OrphansAreThePPID1EnvtestBinaries": {
			parent: 1,
			want:   []int{101, 102, 103, 104},
		},
		"OwnChildrenAreThisBinarysEnvtestBinaries": {
			parent: self,
			want:   []int{201, 202},
		},
		"NoMatchingParent": {
			parent: 7,
			want:   nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var got []int
			for _, p := range selectEnvtestProcesses(procs, tt.parent, binaries) {
				got = append(got, p.PID)
			}

			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("selectEnvtestProcesses() PIDs -want +got:\n%s", diff)
			}
		})
	}
}

func TestStartEnvtestRefusesOnceStopping(t *testing.T) {
	envtestStopping.Store(true)
	t.Cleanup(func() { envtestStopping.Store(false) })

	env := &envtest.Environment{}
	if _, err := startEnvtest(env); err == nil {
		_ = env.Stop()

		t.Fatal("startEnvtest() started envtest after a guard began stopping this binary's servers")
	}
}

func TestEnvtestDeadlineGuardDelay(t *testing.T) {
	tests := map[string]struct {
		timeout   time.Duration
		wantDelay time.Duration
		wantArmed bool
	}{
		"NoTimeout":            {timeout: 0, wantArmed: false},
		"NegativeTimeout":      {timeout: -time.Second, wantArmed: false},
		"GoTestDefault":        {timeout: 10 * time.Minute, wantDelay: 10*time.Minute - 5*time.Second, wantArmed: true},
		"LongTimeout":          {timeout: 60 * time.Minute, wantDelay: 60*time.Minute - 5*time.Second, wantArmed: true},
		"ShortTimeoutScales":   {timeout: 30 * time.Second, wantDelay: 27 * time.Second, wantArmed: true},
		"MarginBoundary":       {timeout: 50 * time.Second, wantDelay: 45 * time.Second, wantArmed: true},
		"TinyTimeoutStillWins": {timeout: 10 * time.Millisecond, wantDelay: 9 * time.Millisecond, wantArmed: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			gotDelay, gotArmed := envtestDeadlineGuardDelay(tt.timeout)
			if gotArmed != tt.wantArmed || gotDelay != tt.wantDelay {
				t.Errorf("envtestDeadlineGuardDelay(%v) = (%v, %v), want (%v, %v)",
					tt.timeout, gotDelay, gotArmed, tt.wantDelay, tt.wantArmed)
			}
		})
	}
}
