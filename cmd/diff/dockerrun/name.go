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
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxContainerNameLength is the maximum length for Docker container names.
// While Docker doesn't enforce a strict limit, DNS hostname compatibility
// and various orchestration tools typically limit names to 63 characters.
const maxContainerNameLength = 63

// containerSuffix is the suffix every function container a run starts carries. It is what identifies
// the container as that run's: the upstream Docker runtime offers no way to label a function container,
// but it does let the caller name it.
func containerSuffix(runID string) string {
	return "-diff-" + runID
}

// containerName creates a stable Docker container name from a function package reference and run ID.
// Example: xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0 with run ID
// "0123456789abcdef" returns function-go-templating-v0.11.0-diff-0123456789abcdef.
//
// For SHA256 digest references, the digest is truncated to 12 characters (similar to Docker short IDs):
// function-go-templating@sha256:54726c28b78f... -> function-go-templating-54726c28b78f-diff-<runID>
//
// If the resulting name exceeds 63 characters, the function name is truncated and a hash of the package
// is added to keep it unique. The run's suffix always survives intact.
func containerName(pkg, runID string) string {
	suffix := containerSuffix(runID)

	if pkg == "" {
		return "unknown" + suffix
	}

	// Format: registry/org/name:version or registry/org/name@sha256:digest. Keep the last path
	// element and make it container-name friendly.
	parts := strings.Split(pkg, "/")
	base := parts[len(parts)-1]

	if before, digest, ok := strings.Cut(base, "@sha256:"); ok {
		// Use the first 12 characters of the digest, like Docker short image IDs.
		base = strings.ReplaceAll(before, ":", "-") + "-" + digest[:min(len(digest), 12)]
	} else {
		base = strings.ReplaceAll(base, ":", "-")
	}

	if len(base)+len(suffix) <= maxContainerNameLength {
		return base + suffix
	}

	// Truncate to respect DNS hostname length limits: <truncated-name>-<hash><suffix>.
	hash := sha256.Sum256([]byte(pkg))
	hashSuffix := "-" + hex.EncodeToString(hash[:])[:8]

	base = strings.TrimRight(base[:max(maxContainerNameLength-len(hashSuffix)-len(suffix), 0)], "-")

	return base + hashSuffix + suffix
}
