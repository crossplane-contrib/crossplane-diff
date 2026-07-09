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

import "testing"

func TestResolveMaxRecvMessageSize(t *testing.T) {
	tests := []struct {
		name string
		flag int
		env  string // "" means env unset
		want int
	}{
		{name: "flag set wins over env", flag: 16, env: "8", want: 16},
		{name: "env fallback when flag unset", flag: 0, env: "8", want: 8},
		{name: "neither set returns zero", flag: 0, env: "", want: 0},
		{name: "non-integer env ignored", flag: 0, env: "notanint", want: 0},
		{name: "zero env ignored", flag: 0, env: "0", want: 0},
		{name: "negative env ignored", flag: 0, env: "-5", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv(envMaxRecvMessageSize, "")
			} else {
				t.Setenv(envMaxRecvMessageSize, tt.env)
			}

			if got := resolveMaxRecvMessageSize(tt.flag); got != tt.want {
				t.Errorf("resolveMaxRecvMessageSize(%d) with env %q = %d, want %d", tt.flag, tt.env, got, tt.want)
			}
		})
	}
}
