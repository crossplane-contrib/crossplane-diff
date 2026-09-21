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
