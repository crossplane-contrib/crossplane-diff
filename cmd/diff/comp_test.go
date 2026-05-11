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
	"reflect"
	"strings"
	"testing"
)

// TestCompCmd_EventualStateField verifies the `comp` command declares an --eventual-state
// boolean Kong flag with the same tag semantics as `xr`. This is the cheapest, most direct
// test of requirement R1: the CLI grammar exposes the flag.
//
// Running this test without the field definition produces a compile error; running it with
// a misconfigured struct tag (wrong name, wrong default) produces a descriptive failure.
func TestCompCmd_EventualStateField(t *testing.T) {
	rt := reflect.TypeFor[CompCmd]()

	f, ok := rt.FieldByName("EventualState")
	if !ok {
		t.Fatalf("CompCmd has no EventualState field")
	}

	if f.Type.Kind() != reflect.Bool {
		t.Errorf("EventualState kind = %v; want bool", f.Type.Kind())
	}

	tag := string(f.Tag)

	wantFragments := []string{
		`name:"eventual-state"`,
		`default:"false"`,
		// Help text is case-sensitive and must mention function-sequencer so `--help`
		// explains the flag's primary use case. Matching the XRCmd tag verbatim.
		`help:"Show eventual state after all reconciliation cycles complete (useful with function-sequencer)."`,
	}
	for _, want := range wantFragments {
		if !strings.Contains(tag, want) {
			t.Errorf("CompCmd.EventualState tag missing %q; got %q", want, tag)
		}
	}
}

// TestCompCmd_HelpMentionsEventualState verifies the comp command's Help() text includes
// an --eventual-state example, mirroring the `xr` command.
func TestCompCmd_HelpMentionsEventualState(t *testing.T) {
	cmd := &CompCmd{}

	help := cmd.Help()
	if !strings.Contains(help, "--eventual-state") {
		t.Errorf("CompCmd.Help() does not mention --eventual-state; got:\n%s", help)
	}
}
