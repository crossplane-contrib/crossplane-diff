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

package dockerrun

import (
	"strings"
	"testing"
)

func TestContainerName(t *testing.T) {
	const runID = "0123456789abcdef"

	tests := map[string]struct {
		pkg  string
		want string
	}{
		"StandardPackage": {
			pkg:  "xpkg.io/crossplane-contrib/function-go-templating:v0.11.0",
			want: "function-go-templating-v0.11.0-diff-0123456789abcdef",
		},
		"DifferentRegistry": {
			pkg:  "ghcr.io/crossplane/function-auto-ready:v1.2.3",
			want: "function-auto-ready-v1.2.3-diff-0123456789abcdef",
		},
		"ShortPackage": {
			pkg:  "function-test:v1.0.0",
			want: "function-test-v1.0.0-diff-0123456789abcdef",
		},
		"NoVersion": {
			pkg:  "xpkg.io/org/function-name",
			want: "function-name-diff-0123456789abcdef",
		},
		"EmptyPackage": {
			pkg:  "",
			want: "unknown-diff-0123456789abcdef",
		},
		"OnlyName": {
			pkg:  "my-function",
			want: "my-function-diff-0123456789abcdef",
		},
		"SHA256Digest": {
			// SHA256 digest is truncated to 12 chars (like Docker short IDs)
			pkg:  "xpkg.io/crossplane-contrib/function-go-templating@sha256:54726c28b78f51a7e88b87db33de79721d8be60890b71dad96276aea4a3397d1",
			want: "function-go-templating-54726c28b78f-diff-0123456789abcdef",
		},
		"SHA256DigestShort": {
			// Short digest (less than 12 chars) is used as-is
			pkg:  "xpkg.io/test/function@sha256:abc123",
			want: "function-abc123-diff-0123456789abcdef",
		},
		"TagAndDigest": {
			pkg:  "registry.io/path/function-auto-ready:v0.6.1-0@sha256:751a4afb65f1abcdef1234567890abcdef1234567890abcdef1234567890abcd",
			want: "function-auto-ready-v0.6.1-0-751a4afb65f1-diff-0123456789abcdef",
		},
		"TruncatedKeepsRunSuffix": {
			// Too long for a DNS label: the function name is cut and a hash of the package keeps it
			// unique, but the suffix the reaper keys on survives intact.
			pkg:  "xpkg.io/org/my-very-long-function-name-that-might-cause-issues@sha256:54726c28b78f51a7e88b87db33de79721d8be60890b71dad96276aea4a3397d1",
			want: "my-very-long-function-name-that-cde05bc1-diff-0123456789abcdef",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := containerName(tt.pkg, runID)
			if got != tt.want {
				t.Errorf("containerName(%q, %q) = %q, want %q", tt.pkg, runID, got, tt.want)
			}

			if len(got) > maxContainerNameLength {
				t.Errorf("containerName(%q, %q) is %d characters, want <= %d", tt.pkg, runID, len(got), maxContainerNameLength)
			}

			if !strings.HasSuffix(got, containerSuffix(runID)) {
				t.Errorf("containerName(%q, %q) = %q, want suffix %q so the run's reaper recognises it",
					tt.pkg, runID, got, containerSuffix(runID))
			}
		})
	}
}
