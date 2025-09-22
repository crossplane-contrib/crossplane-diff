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
package types //nolint:revive // types is an appropriate name for a package containing shared type definitions

import (
	"context"

	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiextensionsv1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
)

// CompositionProvider is a function that provides a composition for a given resource.
type CompositionProvider func(ctx context.Context, res *un.Unstructured) (*apiextensionsv1.Composition, error)
