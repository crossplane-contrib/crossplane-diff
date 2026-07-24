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
// a semantic version at all. A leading "v" is accepted (and optional). It is
// intended for validating an explicitly pinned --crossplane-version; the
// floating :stable tag and full --crossplane-image references carry no
// comparable version and are not checked here.
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
