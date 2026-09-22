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

// Package types defines shared type definitions and interfaces used across the crossplane-diff application.
package types

import (
	"context"

	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

// Verb is a Kubernetes API verb, as it appears in a SelfSubjectAccessReview's
// resourceAttributes. See kubernetes.AccessChecker.
//
// It is a named string rather than a validated enum on purpose, and there is
// nothing upstream to adopt: authorizationv1.ResourceAttributes.Verb is a plain
// string, and the only verb constant in k8s.io/api is rbac's VerbAll ("*").
// That is because the verb space is genuinely open — subresources, plus
// escalate, bind, impersonate, use, and whatever an aggregated API server
// defines — so this type deliberately does not restrict the value.
//
// What it buys is narrow but real. AccessChecker.Can takes a namespace and a
// verb adjacently; while both were plain strings, transposing them compiled
// cleanly and asked the apiserver "may I 'create' in namespace 'create'?",
// which comes back denied and would then be reported as a permission
// limitation — the exact false degradation that authorization check exists to
// prevent. Distinct types make that transposition a compile error. Note it does
// NOT prevent an empty verb, since an untyped "" converts implicitly; that is
// left to tests rather than a runtime guard, there being two call sites and both
// passing a constant below.
//
// It lives in this leaf package rather than beside AccessChecker because
// testutils must name it to implement the mock, and testutils cannot import
// client/kubernetes — that package's tests are internal (package kubernetes)
// and import testutils, so the edge would close a cycle. Same reason
// renderer/types exists. Do not "tidy" this back next to the interface.
type Verb string

const (
	// VerbCreate authorizes a dry-run create, for resources that do not yet exist.
	VerbCreate Verb = "create"

	// VerbPatch authorizes a dry-run server-side apply against a resource that
	// already exists.
	VerbPatch Verb = "patch"
)

// CompositionProvider is a function that provides a composition for a given resource.
type CompositionProvider func(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error)

// FindCompositesOptions narrows what CompositionClient.FindComposites returns.
// Lives here (not in the crossplane client package) so test mocks in cmd/diff/testutils
// can implement the interface without creating an import cycle with cmd/diff/client/crossplane.
type FindCompositesOptions struct {
	// Namespace scopes default discovery to a single namespace. Empty = all namespaces.
	// Ignored when Refs is non-empty (refs carry their own namespace).
	Namespace string
	// Refs limits the result to specific user-named composites. When non-empty, a ref is included
	// in the result only if (a) the named resource exists at the ref's [namespace/]name and (b) it
	// references the supplied composition. Refs that don't satisfy both are silently omitted; the
	// caller derives "unmatched" from the diff between input refs and returned objects.
	Refs []k8stypes.NamespacedName
}
