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

package ref

import (
	"strings"
	"testing"

	k8stypes "k8s.io/apimachinery/pkg/types"
)

func TestParse(t *testing.T) {
	tests := map[string]struct {
		input   string
		want    k8stypes.NamespacedName
		wantErr bool
	}{
		"BareName_ClusterScoped": {
			input: "my-xr",
			want:  k8stypes.NamespacedName{Namespace: "", Name: "my-xr"},
		},
		"NamespaceAndName": {
			input: "default/my-claim",
			want:  k8stypes.NamespacedName{Namespace: "default", Name: "my-claim"},
		},
		"WhitespaceTrimmed": {
			input: "  default/my-claim  ",
			want:  k8stypes.NamespacedName{Namespace: "default", Name: "my-claim"},
		},
		"WhitespaceAroundSlash_TrimmedPerPart": {
			input: "default / my-claim",
			want:  k8stypes.NamespacedName{Namespace: "default", Name: "my-claim"},
		},
		"WhitespaceAroundClusterScopedName_Trimmed": {
			input: "  my-xr  ",
			want:  k8stypes.NamespacedName{Name: "my-xr"},
		},
		"Empty": {
			input:   "",
			wantErr: true,
		},
		"OnlyWhitespace": {
			input:   "   ",
			wantErr: true,
		},
		"EmptyNameAfterSlash": {
			input:   "default/",
			wantErr: true,
		},
		"TooManySlashes": {
			input:   "default/foo/bar",
			wantErr: true,
		},
		"EmptyNamespaceLeadingSlash": {
			input:   "/foo",
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got %+v", tt.input, got)
				}

				if !strings.Contains(err.Error(), tt.input) && tt.input != "" {
					t.Errorf("error message %q should reference offending input %q", err.Error(), tt.input)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tt.input, err)
			}

			if got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseAll(t *testing.T) {
	tests := map[string]struct {
		input         []string
		want          []k8stypes.NamespacedName
		wantErr       bool
		wantErrSubstr string
	}{
		"Empty_NilInput": {
			input: nil,
			want:  nil,
		},
		"Empty_EmptySlice": {
			input: []string{},
			want:  nil,
		},
		"AllValid": {
			input: []string{"foo", "ns/bar", "default/baz"},
			want: []k8stypes.NamespacedName{
				{Name: "foo"},
				{Namespace: "ns", Name: "bar"},
				{Namespace: "default", Name: "baz"},
			},
		},
		"FirstErrorStopsParsing": {
			input:         []string{"good", "/bad", "would-be-good"},
			wantErr:       true,
			wantErrSubstr: "/bad",
		},
		"InvalidEntry": {
			input:         []string{"a/b/c"},
			wantErr:       true,
			wantErrSubstr: "a/b/c",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseAll(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}

				if tt.wantErrSubstr != "" && !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantErrSubstr)
				}

				if got != nil {
					t.Errorf("expected nil result on error, got %+v", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("got %d refs, want %d (%+v vs %+v)", len(got), len(tt.want), got, tt.want)
			}

			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseAll[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFormat(t *testing.T) {
	tests := map[string]struct {
		ref  k8stypes.NamespacedName
		want string
	}{
		"ClusterScoped_BareName": {
			ref:  k8stypes.NamespacedName{Name: "my-xr"},
			want: "my-xr",
		},
		"Namespaced": {
			ref:  k8stypes.NamespacedName{Namespace: "default", Name: "my-claim"},
			want: "default/my-claim",
		},
		"EmptyEverything": {
			ref:  k8stypes.NamespacedName{},
			want: "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Format(tt.ref); got != tt.want {
				t.Errorf("Format(%+v) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}
