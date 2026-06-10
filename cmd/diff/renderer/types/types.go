// Package types provides types used in the renderer in order to facilitate code reuse in test
package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

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

// generatedSuffixLen mirrors the hash length crossplane's
// internal/names.ChildName uses when generating a child name. Names produced
// by `crossplane internal render` for composed resources are
// "<parent-with-trailing-dash><12 lowercase hex>"; we synthesize XR names
// in the same shape (see SynthesizeGeneratedName).
const generatedSuffixLen = 12

// xrSynthesisSeed is the input we hash to derive the 12-hex suffix appended
// to an XR's generateName when synthesizing a metadata.name. Picking
// upstream-shape (12 hex via sha256, see SynthesizeGeneratedName) means the
// resulting XR name matches the shape of binary-generated composed names AND
// the suffix value itself is recognisable on its own — which we rely on for
// downstream detection in LooksLikeGeneratedName.
const xrSynthesisSeed = "crossplane-diff/synthesized-xr"

// xrSynthesisSuffix returns the deterministic 12-hex suffix we use for XR
// name synthesis. Two surfaces depend on it:
//
//  1. Direct shape match — the synthesized XR name itself is
//     "<gen-with-dash><suffix>", caught by LooksLikeGeneratedName via the
//     shape branch.
//  2. Embedded match — composition templates that interpolate the XR's
//     metadata.name into a downstream resource's name carry the suffix
//     verbatim (e.g. "<gen-with-dash><suffix>" or
//     "<gen-with-dash><suffix>-<binary-suffix>"); those resources may not
//     have their own generateName set, so we detect via Contains.
func xrSynthesisSuffix() string {
	h := sha256.Sum256([]byte(xrSynthesisSeed))
	return hex.EncodeToString(h[:])[:generatedSuffixLen]
}

// SynthesizeGeneratedName builds a metadata.name in the same shape upstream's
// internal/names.ChildName produces: "<parent-with-trailing-dash><12 hex>".
//
// We use this when an XR has bare generateName and we need a metadata.name
// for the binary's validation. Picking upstream-shape lets the same
// detector (LooksLikeGeneratedName) catch both this name and the binary's
// own composed-resource names.
func SynthesizeGeneratedName(parent string) string {
	if !strings.HasSuffix(parent, "-") {
		parent += "-"
	}

	return parent + xrSynthesisSuffix()
}

// LooksLikeGeneratedName reports whether name was produced by either our
// XR synthesis path or the binary's nameGenerator. True when:
//
//   - name has the deterministic shape upstream's nameGenerator emits —
//     "<generateName-with-dash><12 lowercase hex>" (catches binary-generated
//     composed-resource names whose template carries a generateName); or
//   - name embeds xrSynthesisSuffix anywhere (catches the synthesized XR
//     itself and any downstream resource whose template interpolated the
//     XR's metadata.name into its own).
func LooksLikeGeneratedName(name, generateName string) bool {
	if name == "" {
		return false
	}

	if generateName != "" {
		gen := generateName
		if !strings.HasSuffix(gen, "-") {
			gen += "-"
		}

		if suffix, ok := strings.CutPrefix(name, gen); ok && isHex(suffix, generatedSuffixLen) {
			return true
		}
	}

	return strings.Contains(name, xrSynthesisSuffix())
}

// GeneratedDisplayName returns the user-facing label for a name produced by
// either synthesis path. Caller is responsible for checking
// LooksLikeGeneratedName first.
func GeneratedDisplayName(name, generateName string) string {
	if generateName != "" {
		gen := generateName
		if !strings.HasSuffix(gen, "-") {
			gen += "-"
		}

		if suffix, ok := strings.CutPrefix(name, gen); ok && isHex(suffix, generatedSuffixLen) {
			return generateName + "(generated)"
		}
	}

	// xrSynthesisSuffix was interpolated into name; cut at the suffix and
	// drop the leading dash from what remains so the display reads cleanly.
	before, _, _ := strings.Cut(name, xrSynthesisSuffix())

	return strings.TrimSuffix(before, "-") + "-(generated)"
}

func isHex(s string, length int) bool {
	if len(s) != length {
		return false
	}

	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return true
}

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

// DiffKey constants for structured diff output.
// These are the keys used in the diff map to hold resource states.
const (
	DiffKeyOld  = "old"  // Current state for modified resources
	DiffKeyNew  = "new"  // Desired state for modified resources
	DiffKeySpec = "spec" // Full spec for added/removed resources
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
