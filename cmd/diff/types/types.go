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

	corev1 "k8s.io/api/core/v1"
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

// CompositionProvider resolves the composition a composite is rendered against, and the
// CompositionRevision that composition comes from.
type CompositionProvider func(ctx context.Context, res *un.Unstructured) (ResolvedComposition, error)

// ResolvedComposition is what a CompositionProvider resolves for one composite.
type ResolvedComposition struct {
	// Composition is the composition to render the composite against.
	Composition *apiextensionsv1.Composition

	// RevisionName is the name of the CompositionRevision Composition comes from. Crossplane's composite
	// reconciler writes the revision it selects to the composite's compositionRevisionRef before it
	// composes, so the composite is rendered with its ref set to this name, and a template reading the
	// revision name sees the revision that rendered it (#536). Empty means unknown, for example a
	// composition with no revisions yet. The composite then keeps the ref it has; a name is never made up.
	RevisionName string
}

// ValidatedInput is one input an input validator hands back for rendering: the resource, and the error
// that rejected it before rendering, if any. Lives here (not in diffprocessor, beside the InputValidator
// interface that returns it) so the validator's mock in cmd/diff/testutils can name it without an import
// cycle.
type ValidatedInput struct {
	Resource *un.Unstructured
	Err      error // non-nil if rejected before rendering
}

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

// CredentialFetchResult reports the outcome of fetching a composition's function-credential secrets
// from the cluster. Lives here for the same reason as FindCompositesOptions: so test mocks in
// cmd/diff/testutils can implement CredentialClient without an import cycle.
//
// Absent secrets are reported rather than warned about at the point of discovery, because whether an
// absent secret is a problem depends on something the client cannot see: the caller may have supplied
// it via --function-credentials. Only the caller, after merging its own credentials in, can tell which
// shortfalls actually remain.
type CredentialFetchResult struct {
	// Secrets are the credential secrets successfully read from the cluster, in pipeline order.
	Secrets []corev1.Secret

	// Absent identifies the credential secrets a pipeline step references that do not exist on the
	// cluster, in pipeline order and deduplicated. A NotFound is the only fetch failure recorded here;
	// every other failure is returned as an error, because it leaves the credential state unknown
	// rather than known-empty.
	Absent []k8stypes.NamespacedName
}
