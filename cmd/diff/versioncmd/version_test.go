/*
Copyright 2023 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package versioncmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"k8s.io/client-go/rest"
)

// newKongCtx returns a kong.Context wired with an in-memory stdout buffer so
// tests can assert command output without touching the real stdout.
func newKongCtx(buf *bytes.Buffer) *kong.Context {
	return &kong.Context{
		Kong: &kong.Kong{Stdout: buf},
	}
}

func TestCmd_Run_ClientOnly(t *testing.T) {
	var buf bytes.Buffer

	cmd := &Cmd{
		Client: true,
		// Stub fetcher that would fail the test if called — --client must short-circuit.
		fetch: func(context.Context, *rest.Config) (string, error) {
			t.Fatal("fetcher should not be called when --client is set")
			return "", nil
		},
	}

	if err := cmd.Run(newKongCtx(&buf)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Client Version:") {
		t.Errorf("output %q missing 'Client Version:'", out)
	}

	if strings.Contains(out, "Server Version:") {
		t.Errorf("output %q should not contain 'Server Version:' when --client is set", out)
	}
}

func TestCmd_Run_ServerVersion(t *testing.T) {
	var buf bytes.Buffer

	called := false
	cmd := &Cmd{
		fetch: func(_ context.Context, _ *rest.Config) (string, error) {
			called = true
			return "v2.0.2", nil
		},
	}

	// Point KUBECONFIG at a temp (empty) file to short-circuit real kubeconfig
	// loading. The stub above means we don't actually care what config is
	// returned, as long as the flow gets there.
	withTempKubeconfig(t)

	if err := cmd.Run(newKongCtx(&buf)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	out := buf.String()

	if !called {
		t.Error("fetcher was not called")
	}

	if !strings.Contains(out, "Client Version:") {
		t.Errorf("output %q missing 'Client Version:'", out)
	}

	if !strings.Contains(out, "Server Version: v2.0.2") {
		t.Errorf("output %q missing 'Server Version: v2.0.2'", out)
	}
}

func TestCmd_Run_ServerFetchErrorIsWrapped(t *testing.T) {
	var buf bytes.Buffer

	cmd := &Cmd{
		fetch: func(_ context.Context, _ *rest.Config) (string, error) {
			return "", errors.New("boom")
		},
	}

	withTempKubeconfig(t)

	err := cmd.Run(newKongCtx(&buf))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), errGetCrossplaneVersion) {
		t.Errorf("error %q missing wrapper %q", err, errGetCrossplaneVersion)
	}

	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q missing underlying cause 'boom'", err)
	}
}

func TestCmd_Run_ServerVersionEmptyDoesNotPrint(t *testing.T) {
	// If the fetcher returns an empty string without error, we should not
	// print a bogus "Server Version: " line.
	var buf bytes.Buffer

	cmd := &Cmd{
		fetch: func(context.Context, *rest.Config) (string, error) { return "", nil },
	}

	withTempKubeconfig(t)

	if err := cmd.Run(newKongCtx(&buf)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if strings.Contains(buf.String(), "Server Version:") {
		t.Errorf("output %q unexpectedly contains 'Server Version:'", buf.String())
	}
}

func TestCmd_ContextFlag_Parses(t *testing.T) {
	var cli struct {
		Version Cmd `cmd:""`
	}

	parser, err := kong.New(&cli)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	if _, err := parser.Parse([]string{"version", "--context", "foo", "--client"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cli.Version.Context != "foo" {
		t.Errorf("Context = %q, want %q", cli.Version.Context, "foo")
	}

	if !cli.Version.Client {
		t.Error("expected --client to parse as true")
	}
}

func TestCmd_GetKubeContext(t *testing.T) {
	cmd := &Cmd{Context: "staging"}
	if got := cmd.GetKubeContext(); got != "staging" {
		t.Errorf("GetKubeContext() = %q, want %q", got, "staging")
	}
}

// withTempKubeconfig points $KUBECONFIG at an empty temp dir entry so
// kubecfg.Provide returns a resolvable (albeit dummy) config when tests run
// without caring about what the resulting REST config points at. It isolates
// the test from the developer's real ~/.kube/config.
func withTempKubeconfig(t *testing.T) {
	t.Helper()

	const yaml = `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:0
contexts:
- name: default
  context:
    cluster: default
    user: default
users:
- name: default
  user: {}
`

	dir := t.TempDir()

	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	t.Setenv("KUBECONFIG", path)
}
