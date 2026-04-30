/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package kubecfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const twoContextKubeconfig = `apiVersion: v1
kind: Config
current-context: ctx-a
clusters:
- name: cluster-a
  cluster:
    server: https://a.example.com
- name: cluster-b
  cluster:
    server: https://b.example.com
contexts:
- name: ctx-a
  context:
    cluster: cluster-a
    user: user-a
- name: ctx-b
  context:
    cluster: cluster-b
    user: user-b
users:
- name: user-a
  user: {}
- name: user-b
  user: {}
`

type staticProvider struct{ ctx Context }

func (s staticProvider) GetKubeContext() Context { return s.ctx }

func writeTempKubeconfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	t.Setenv("KUBECONFIG", path)

	return path
}

func TestProvide_DefaultContext(t *testing.T) {
	writeTempKubeconfig(t)

	cfg, err := Provide(staticProvider{ctx: ""})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}

	if cfg.Host != "https://a.example.com" {
		t.Errorf("Host = %q, want https://a.example.com (current context)", cfg.Host)
	}

	if cfg.QPS != 20 {
		t.Errorf("QPS = %v, want 20", cfg.QPS)
	}

	if cfg.Burst != 30 {
		t.Errorf("Burst = %v, want 30", cfg.Burst)
	}
}

func TestProvide_ContextOverride(t *testing.T) {
	writeTempKubeconfig(t)

	cfg, err := Provide(staticProvider{ctx: "ctx-b"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}

	if cfg.Host != "https://b.example.com" {
		t.Errorf("Host = %q, want https://b.example.com (overridden context)", cfg.Host)
	}
}

func TestProvide_EmptyConfigFallbackInCluster(t *testing.T) {
	// Point KUBECONFIG at a nonexistent file so clientcmd returns ErrEmptyConfig.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("HOME", t.TempDir())

	var warned string

	stubCluster := &rest.Config{Host: "https://in-cluster.example.com"}

	cfg, err := provide(staticProvider{ctx: ""}, func() (*rest.Config, error) {
		return stubCluster, nil
	}, func(msg string) { warned = msg })
	if err != nil {
		t.Fatalf("provide: %v", err)
	}

	if cfg.Host != "https://in-cluster.example.com" {
		t.Errorf("Host = %q, want in-cluster host", cfg.Host)
	}

	if !strings.Contains(warned, "in-cluster") {
		t.Errorf("warning = %q, expected it to mention in-cluster", warned)
	}
}

func TestProvide_EmptyConfigAndInClusterFailsReturnsOriginal(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("HOME", t.TempDir())

	inClusterErr := errors.New("no service account token")

	_, err := provide(staticProvider{ctx: ""}, func() (*rest.Config, error) {
		return nil, inClusterErr
	}, func(string) {})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The error we surface should be the clientcmd empty-config error, not the in-cluster error.
	if !clientcmd.IsEmptyConfig(err) {
		t.Errorf("expected empty-config error to be preserved, got: %v", err)
	}
}
