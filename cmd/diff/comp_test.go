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

	dp "github.com/crossplane-contrib/crossplane-diff/cmd/diff/diffprocessor"
)

func TestCompCmd_ValidateFlags(t *testing.T) {
	tests := map[string]struct {
		cmd            CompCmd
		wantErr        bool
		errMustContain []string
		// wantAnalyzeOn is the value analyzeOn() must resolve to on a successful validation. Its zero
		// value is the empty string, which is what "--analyze-on not passed" looks like: the CLI
		// leaves the processor default (any-change) in place rather than restating it here.
		wantAnalyzeOn dp.AnalyzeOn
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
		// The deprecated flag on its own has to keep working, and it is the reason --analyze-on's kong
		// default is empty rather than "any-change": kong cannot tell a defaulted flag from an
		// explicitly-passed one, so a non-empty default would make every --analyze-unchanged
		// invocation look like a conflict.
		"OnlyAnalyzeUnchanged": {
			cmd:           CompCmd{AnalyzeUnchanged: true},
			wantAnalyzeOn: dp.AnalyzeOnAlways,
		},
		"OnlyAnalyzeOn": {
			cmd:           CompCmd{AnalyzeOn: string(dp.AnalyzeOnSpecChange)},
			wantAnalyzeOn: dp.AnalyzeOnSpecChange,
		},
		// Both flags asking for the same setting is redundant, not contradictory, so it is allowed.
		"AnalyzeUnchangedAgreesWithAnalyzeOn": {
			cmd:           CompCmd{AnalyzeUnchanged: true, AnalyzeOn: string(dp.AnalyzeOnAlways)},
			wantAnalyzeOn: dp.AnalyzeOnAlways,
		},
		// Two flags asking for the same setting and disagreeing: preferring either one silently would
		// give the user analysis they did not ask for, or withhold analysis they did.
		"AnalyzeUnchangedConflictsWithAnalyzeOn": {
			cmd:            CompCmd{AnalyzeUnchanged: true, AnalyzeOn: string(dp.AnalyzeOnSpecChange)},
			wantErr:        true,
			errMustContain: []string{"--analyze-unchanged", "--analyze-on=spec-change"},
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

			if got := tt.cmd.analyzeOn(); got != tt.wantAnalyzeOn {
				t.Errorf("analyzeOn() = %q, want %q", got, tt.wantAnalyzeOn)
			}
		})
	}
}
