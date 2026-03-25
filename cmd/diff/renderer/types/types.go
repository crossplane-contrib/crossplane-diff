// Package types provides types used in the renderer in order to facilitate code reuse in test
package types

import (
	"fmt"

	"github.com/sergi/go-diff/diffmatchpatch"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResourceDiff represents the diff for a specific resource.
type ResourceDiff struct {
	Gvk          schema.GroupVersionKind
	Namespace    string
	ResourceName string
	DiffType     DiffType
	LineDiffs    []diffmatchpatch.Diff
	Current      *un.Unstructured // Optional, for reference
	Desired      *un.Unstructured // Optional, for reference
}

// DiffType represents the type of diff (added, removed, modified).
type DiffType string

const (
	// DiffTypeAdded an added section.
	DiffTypeAdded DiffType = "+"
	// DiffTypeRemoved a removed section.
	DiffTypeRemoved DiffType = "-"
	// DiffTypeModified a modified section.
	DiffTypeModified DiffType = "~"
	// DiffTypeEqual an unchanged section.
	DiffTypeEqual DiffType = "="
)

// DiffTypeWord constants for human-readable JSON output.
// These are used in structured output (JSON/YAML) for better readability.
const (
	DiffTypeWordAdded    = "added"
	DiffTypeWordRemoved  = "removed"
	DiffTypeWordModified = "modified"
	DiffTypeWordEqual    = "equal"
)

// ToWord converts a DiffType symbol to its human-readable word.
func (d DiffType) ToWord() string {
	switch d {
	case DiffTypeAdded:
		return DiffTypeWordAdded
	case DiffTypeRemoved:
		return DiffTypeWordRemoved
	case DiffTypeModified:
		return DiffTypeWordModified
	case DiffTypeEqual:
		return DiffTypeWordEqual
	default:
		return string(d)
	}
}

// Colors for terminal output.
const (
	// ColorRed an ANSI "begin red" character.
	ColorRed = "\x1b[31m"
	// ColorGreen an ANSI "begin green" character.
	ColorGreen = "\x1b[32m"
	// ColorYellow an ANSI "begin yellow" character.
	ColorYellow = "\x1b[33m"
	// ColorReset an ANSI "reset color" character.
	ColorReset = "\x1b[0m"
)

// GetDiffKey returns a key that can be used to identify this object for use in a map.
func (d *ResourceDiff) GetDiffKey() string {
	return MakeDiffKey(d.Gvk.GroupVersion().String(), d.Gvk.Kind, d.Namespace, d.ResourceName)
}

// MakeDiffKey creates a unique key for a resource diff.
// Format: apiVersion/kind/namespace/name (namespace may be empty for cluster-scoped resources).
func MakeDiffKey(apiVersion, kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", apiVersion, kind, namespace, name)
}

// MakeDiffKeyFromResource creates a unique key for a resource diff from an Unstructured resource.
// This is a convenience wrapper around MakeDiffKey that extracts all fields from the resource.
func MakeDiffKeyFromResource(res *un.Unstructured) string {
	return MakeDiffKey(res.GetAPIVersion(), res.GetKind(), res.GetNamespace(), res.GetName())
}

// OutputError represents an error in structured output.
// Used consistently by both XR diff and comp diff for machine-readable error handling.
// Note: Only JSON tags are used because sigs.k8s.io/yaml uses JSON tags for YAML serialization.
type OutputError struct {
	ResourceID string `json:"resourceID,omitempty"`
	Message    string `json:"message"`
}

// FormatError returns a human-readable error string.
// If ResourceID is empty, it uses "<global>" to indicate a system-level error
// not tied to any specific resource (e.g., cluster connection issues).
func (e OutputError) FormatError() string {
	resourceID := e.ResourceID
	if resourceID == "" {
		resourceID = "<global>"
	}

	return fmt.Sprintf("ERROR: %s: %s", resourceID, e.Message)
}
