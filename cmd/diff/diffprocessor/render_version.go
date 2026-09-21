/*
Copyright 2026 The Crossplane Authors.

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

package diffprocessor

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// MinCrossplaneRenderVersion is the minimum crossplane render version that
// crossplane-diff supports. Renders against older versions silently drop
// cluster-observed composed resources (their controller-ref UIDs no longer
// match the XR the way this tool assembles them), producing incorrect diffs —
// see crossplane-contrib/crossplane-diff#399. The value corresponds to the
// upstream release that preserves a non-empty input XR UID and validates
// observed resources (crossplane/crossplane#7544).
const MinCrossplaneRenderVersion = "v2.3.4"

// minRenderVersion is MinCrossplaneRenderVersion parsed once at package init.
// semver.MustParse panics if the constant is not valid semver — that is a
// compile-time-constant programming error, caught immediately on first import
// rather than deferred to a per-call error branch.
//
//nolint:gochecknoglobals // Parsed-once view of the MinCrossplaneRenderVersion constant; immutable.
var minRenderVersion = semver.MustParse(MinCrossplaneRenderVersion)

// ValidateMinRenderVersion returns an error if version parses to a semantic
// version older than MinCrossplaneRenderVersion, or if it cannot be parsed as
// a semantic version at all. A leading "v" is accepted (and optional — see
// NormalizeRenderVersion, which is what makes the bare form usable rather than
// merely accepted). It is intended for validating an explicitly pinned
// --crossplane-version; a full --crossplane-image reference goes through
// ValidateMinRenderImage instead, and the floating :stable default carries no
// comparable version at all.
func ValidateMinRenderVersion(version string) error {
	v, err := semver.NewVersion(version)
	if err != nil {
		return errors.Wrapf(err, "cannot parse crossplane render version %q as a semantic version", version)
	}

	if v.LessThan(minRenderVersion) {
		return errors.Errorf("crossplane render version %q is below the minimum supported version %s", version, MinCrossplaneRenderVersion)
	}

	return nil
}

// NormalizeRenderVersion returns version with a leading "v" when it parses as a
// semantic version without one, and version unchanged otherwise.
//
// ValidateMinRenderVersion deliberately accepts a bare "2.3.4", but upstream
// formats the pinned version into the image tag verbatim
// (…/crossplane:<version>) and publishes only v-prefixed tags, so without this
// the bare form validates and then dies at pull time as an unpullable image.
// The original string is prefixed rather than reformatted from the parsed value,
// so prerelease and build metadata survive byte-for-byte. Tags that are not
// semantic versions ("stable", "main") are real, pullable tags and are left
// alone.
func NormalizeRenderVersion(version string) string {
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}

	if _, err := semver.NewVersion(version); err != nil {
		return version
	}

	return "v" + version
}

// ValidateMinRenderImage returns an error if the tag of a full crossplane render
// image reference parses as a semantic version older than
// MinCrossplaneRenderVersion.
//
// A reference whose version is not comparable — pinned by digest, tagged with a
// floating name like "stable", or carrying no tag at all — is accepted: refusing
// it would break the mirrored and air-gapped registries --crossplane-image
// exists to serve, and a digest is opaque by construction. Those references are
// reported by UncomparableRenderImageWarning instead, so the floor stays visible
// without becoming a false positive.
func ValidateMinRenderImage(image string) error {
	v, tag, ok := comparableRenderImageVersion(image)
	if !ok {
		return nil
	}

	if v.LessThan(minRenderVersion) {
		return errors.Errorf("crossplane render image %q is tagged %q, which is below the minimum supported version %s", image, tag, MinCrossplaneRenderVersion)
	}

	return nil
}

// UncomparableRenderImageWarning returns advisory text when image is a reference
// whose version cannot be compared against MinCrossplaneRenderVersion, or "" for
// an empty image (no override was requested) or one that ValidateMinRenderImage
// could check. Rendering with an image below the minimum silently drops
// cluster-observed composed resources, which shrinks the rendered set and
// surfaces as spurious removals, so an unverifiable reference is worth saying
// out loud even though it cannot be rejected.
func UncomparableRenderImageWarning(image string) string {
	if image == "" {
		return ""
	}

	if _, _, ok := comparableRenderImageVersion(image); ok {
		return ""
	}

	return fmt.Sprintf("cannot check crossplane render image %q against the minimum supported version %s: the reference carries no comparable version. "+
		"Ensure it resolves to %s or newer — older renders silently drop cluster-observed composed resources, which surfaces as spurious removals in the diff.",
		image, MinCrossplaneRenderVersion, MinCrossplaneRenderVersion)
}

// comparableRenderImageVersion parses the tag of a container image reference as
// a semantic version. It reports ok=false when the reference carries no tag that
// can be compared, which is the case for a digest-pinned reference, a floating
// or branch tag, or a bare repository path.
func comparableRenderImageVersion(image string) (*semver.Version, string, bool) {
	tag := renderImageTag(image)
	if tag == "" {
		return nil, "", false
	}

	v, err := semver.NewVersion(tag)
	if err != nil {
		return nil, "", false
	}

	return v, tag, true
}

// renderImageTag returns the tag portion of a container image reference, or ""
// when there is none.
//
// A reference pinned by digest returns "" even when it also carries a tag: the
// digest is what the daemon resolves, so the tag is advisory and must not be
// treated as authoritative — least of all as grounds for rejection.
func renderImageTag(image string) string {
	// An "@" in a reference can only introduce a digest.
	if strings.Contains(image, "@") {
		return ""
	}

	i := strings.LastIndex(image, ":")
	if i < 0 {
		return ""
	}

	tag := image[i+1:]

	// A ":" that precedes a "/" is a registry port, not a tag separator.
	if strings.Contains(tag, "/") {
		return ""
	}

	return tag
}
