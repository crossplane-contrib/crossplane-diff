/*
Copyright 2025 The Crossplane Authors.

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

package main

import (
	"strings"
	"testing"
)

func TestCompCmd_ValidateFlags(t *testing.T) {
	tests := map[string]struct {
		cmd            CompCmd
		wantErr        bool
		errMustContain []string
	}{
		"NeitherSet": {
			cmd: CompCmd{},
		},
		"OnlyNamespace": {
			cmd: CompCmd{Namespace: "default"},
		},
		"OnlyResources": {
			cmd: CompCmd{Resources: []string{"default/foo"}},
		},
		"BothSet": {
			cmd:            CompCmd{Namespace: "default", Resources: []string{"default/foo"}},
			wantErr:        true,
			errMustContain: []string{"--namespace", "--resource"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.cmd.validateFlags()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}

				for _, sub := range tt.errMustContain {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q must contain %q", err.Error(), sub)
					}
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
