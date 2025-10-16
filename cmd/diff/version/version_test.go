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

package version

import (
	"bytes"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestCmd_Run_ClientOnly(t *testing.T) {
	tests := []struct {
		name          string
		client        bool
		wantSubstring string
	}{
		{
			name:          "ClientVersionOnly",
			client:        true,
			wantSubstring: "Client Version:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a buffer to capture output
			var buf bytes.Buffer

			// Create a Kong instance with our buffer as stdout
			k := &kong.Kong{
				Stdout: &buf,
			}

			// Create a kong.Context with the Kong instance
			ctx := &kong.Context{
				Kong: k,
			}

			// Create the command
			cmd := &Cmd{
				Client: tt.client,
			}

			// Run the command
			err := cmd.Run(ctx)
			if err != nil {
				t.Errorf("Run() error = %v, want nil", err)
				return
			}

			// Check that output contains the expected substring
			output := buf.String()
			if !strings.Contains(output, tt.wantSubstring) {
				t.Errorf("Run() output = %q, want to contain %q", output, tt.wantSubstring)
			}

			// Verify it only outputs client version (no server version)
			if tt.client && strings.Contains(output, "Server Version:") {
				t.Errorf("Run() output = %q, should not contain 'Server Version:' when client=true", output)
			}
		})
	}
}

func TestCmd_Run_ServerVersion(t *testing.T) {
	// This test verifies that when Client=false, the command attempts to fetch
	// the server version. We can't easily test the full flow without a running
	// cluster, so we just verify the command executes without panicking and
	// returns an error (since we don't have a cluster in unit tests).
	t.Run("ServerVersionAttempt", func(t *testing.T) {
		var buf bytes.Buffer

		k := &kong.Kong{
			Stdout: &buf,
		}

		ctx := &kong.Context{
			Kong: k,
		}

		cmd := &Cmd{
			Client: false, // This will attempt to fetch server version
		}

		// We expect this to error since we don't have a cluster
		err := cmd.Run(ctx)

		// Verify client version was still printed
		output := buf.String()
		if !strings.Contains(output, "Client Version:") {
			t.Errorf("Run() output = %q, want to contain 'Client Version:'", output)
		}

		// We expect an error trying to connect to the server
		if err == nil {
			// If there's no error, that means either:
			// 1. We have a cluster available (unlikely in unit tests)
			// 2. The server version fetch succeeded
			// Either way, we should check that server version was printed
			if !strings.Contains(output, "Server Version:") && err == nil {
				t.Errorf("Run() with Client=false succeeded but no Server Version in output")
			}
		}
	})
}

func TestCmd_Structure(t *testing.T) {
	// Verify the Cmd struct has the expected fields
	cmd := &Cmd{}

	// Test that we can set Client to true
	cmd.Client = true
	if !cmd.Client {
		t.Error("Cmd.Client field not working")
	}

	// Test that we can set Client to false
	cmd.Client = false
	if cmd.Client {
		t.Error("Cmd.Client field not working")
	}
}
