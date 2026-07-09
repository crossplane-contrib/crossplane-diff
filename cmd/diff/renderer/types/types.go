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

// ResourceViews holds the two representations of a single resource involved in
// a diff: the Raw object (as rendered, or as fetched from the cluster) and the
// Clean object (Raw after cleanupForDiff has stripped ignored paths and
// server-side/non-diff-relevant fields).
//
// Raw is load-bearing beyond rendering (removal detection and XR
// reconstruction in the diffprocessor). Clean is what structured output emits,
// and is populated by diff generation only when there is something to render
// (i.e. not for equal diffs). Either field may be nil: for an added resource
// the current side is zero-valued, for a removed resource the desired side is.
type ResourceViews struct {
	Raw   *un.Unstructured
	Clean *un.Unstructured
}

// ResourceDiff represents the diff for a specific resource.
type ResourceDiff struct {
	Gvk          schema.GroupVersionKind
	Namespace    string
	ResourceName string
	DiffType     DiffType
	LineDiffs    []diffmatchpatch.Diff
	Current      ResourceViews // the resource's current (cluster) state, raw + clean
	Desired      ResourceViews // the resource's desired (rendered) state, raw + clean
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

		if suffix, ok := strings.CutPrefix(name, gen); ok && isLowerHex(suffix, generatedSuffixLen) {
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

		if suffix, ok := strings.CutPrefix(name, gen); ok && isLowerHex(suffix, generatedSuffixLen) {
			return generateName + "(generated)"
		}
	}

	// xrSynthesisSuffix was interpolated into name; cut at the suffix and
	// drop the leading dash from what remains so the display reads cleanly.
	before, _, _ := strings.Cut(name, xrSynthesisSuffix())

	return strings.TrimSuffix(before, "-") + "-(generated)"
}

// isLowerHex reports whether s is exactly length characters of lowercase
// hex — matching what hex.EncodeToString produces. encoding/hex.DecodeString
// alone accepts uppercase too, so we lowercase-check first; otherwise a
// stdlib decode handles the rest.
func isLowerHex(s string, length int) bool {
	if len(s) != length || s != strings.ToLower(s) {
		return false
	}

	_, err := hex.DecodeString(s)

	return err == nil
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
//
// ResourceID and ValidationFailures play complementary roles:
//
//   - ResourceID identifies the input the diff command was processing
//     (the XR or claim file the user fed in). It is "<Kind>/<Name>"
//     so machine consumers can correlate an error to a specific
//     input across batched runs.
//
//   - ValidationFailures, when non-empty, gives a structured
//     per-resource breakdown of *what* the schema validator rejected
//     within that input's render tree. It contains one entry per
//     resource (the input itself plus any composed resource) that
//     failed validation, with full GVK / namespace / typed errors,
//     so consumers can drive UI or programmatic decisions without
//     parsing the human-readable Message.
//
// The two fields therefore partially overlap when the input itself is
// among the failing resources — that's intentional. ValidationFailures
// is the complete failure list (so consumers iterating it never miss
// an XR-level rejection); ResourceID independently anchors the error
// to a user-supplied input. ValidationFailures is set only for schema
// validation errors; it is nil for tool, IO, and render errors.
type OutputError struct {
	ResourceID         string                      `json:"resourceID,omitempty"`
	Message            string                      `json:"message"`
	ValidationFailures []ResourceValidationFailure `json:"validationFailures,omitempty"`
}

// ResourceValidationFailure is the per-resource view inside an
// OutputError.ValidationFailures slice. It mirrors the shape of
// crossplane/cli's pkg/validate.ResourceValidationResult so the
// information transfers cleanly, but is defined here so crossplane-diff's
// JSON output schema is owned by us — consumers depend on this struct,
// not the upstream type.
type ResourceValidationFailure struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	// Status is the validator-assigned outcome for this resource.
	// Today the surfaced values are "invalid" and "missingSchema";
	// "valid" entries are filtered out by the converter so machine
	// consumers iterating ValidationFailures see only the failure rows.
	Status string                 `json:"status"`
	Errors []FieldValidationError `json:"errors,omitempty"`
}

// FieldValidationError is the wire shape for a single field-level
// validation error. Mirrors pkg/validate.FieldValidationError; defined
// here so consumers of our JSON output bind to a stable schema we
// control rather than the upstream type.
type FieldValidationError struct {
	// Type categorizes the error: "schema", "cel", "unknownField",
	// or "defaulting".
	Type string `json:"type"`
	// Field is the JSONPath of the offending field (e.g.
	// "spec.forProvider.region"), set when the validator can pinpoint
	// a path. Empty for errors with no field locality (e.g.
	// defaulting failures).
	Field string `json:"field,omitempty"`
	// Message is the validator-emitted human-readable description.
	// For k8s-derived schema errors this typically embeds the field
	// path and bad value already.
	Message string `json:"message"`
	// Value, when set, is the offending value as the validator saw
	// it. Type is preserved (string, number, bool, struct), so JSON
	// consumers can present or compare it directly.
	Value any `json:"value,omitempty"`
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
