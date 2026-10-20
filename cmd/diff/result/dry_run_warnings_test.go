package result

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	forbiddenSummary = "skipped apiserver verification of added resources: not authorized to create them, so their diffs omit server-side defaulting and admission"
	namespaceSummary = "skipped apiserver verification of added resources: their namespace does not exist yet, so their diffs omit server-side defaulting and admission"
	webhookSummary   = "skipped apiserver verification of added resources: the cluster could not complete admission"
	storageNote      = "; they are stored at a version other than the one requested, whose defaults and conversion the cluster may apply but their diffs do not"
	disabledSummary  = "did not verify added resources against the apiserver (--dry-run-on=existing)"
	testResourceGVK  = "example.org/v1, Kind=TestResource"
	otherResourceGVK = "example.org/v1, Kind=OtherResource"
)

// unverifiedAddition builds the diff of an added resource whose dry-run create was skipped for reason,
// with the cluster's explanation detail.
func unverifiedAddition(kind, name, namespace string, reason DryRunSkipReason, detail string) *ResourceDiff {
	return &ResourceDiff{
		Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: kind},
		Namespace:    namespace,
		ResourceName: name,
		DiffType:     DiffTypeAdded,
		DryRun:       &DryRunInfo{SkipReason: reason, Detail: detail},
	}
}

// storedAt makes d an addition requested at version and stored, by its CRD, at storageVersion.
func storedAt(d *ResourceDiff, version, storageVersion string) *ResourceDiff {
	d.Gvk.Version = version
	d.DryRun.StorageVersion = storageVersion

	return d
}

// diffMap keys diffs the way the diff calculator does.
func diffMap(diffs ...*ResourceDiff) map[string]*ResourceDiff {
	out := make(map[string]*ResourceDiff, len(diffs))
	for _, d := range diffs {
		out[d.GetDiffKey()] = d
	}

	return out
}

// TestDryRunWarnings covers how the advisory for unverified additions is derived from the DryRunInfo
// on the diffs: one warning per GVK + namespace + distinct cause, counting the resources behind it, and
// no cause ever dropped. The text renderer shows no DryRunInfo, so this warning is the only place a
// human learns that an addition was not verified, or why.
func TestDryRunWarnings(t *testing.T) {
	tests := map[string]struct {
		reason   string
		diffSets []map[string]*ResourceDiff
		want     []OutputWarning
	}{
		"ForbiddenCollapsesPerGVKAndNamespace": {
			reason: "Four denied additions across three GVK+namespace groups raise three warnings, each counting its resources.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipForbidden, "no create"),
				unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipForbidden, "no create"),
				unverifiedAddition("TestResource", "c", "ns-b", DryRunSkipForbidden, "no create"),
				unverifiedAddition("OtherResource", "d", "ns-a", DryRunSkipForbidden, "no create"),
			)},
			want: []OutputWarning{
				{Message: forbiddenSummary, Context: map[string]string{"gvk": otherResourceGVK, "namespace": "ns-a", "reason": "no create", "count": "1"}},
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "reason": "no create", "count": "2"}},
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-b", "reason": "no create", "count": "1"}},
			},
		},
		"DistinctCausesAreAllKept": {
			reason: "Two different apiserver causes in one GVK+namespace group are two warnings. Grouping on reason alone would keep one and silently drop the other.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipWebhookUnavailable, `failed calling webhook "policy.example.org"`),
				unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipWebhookUnavailable, `failed calling webhook "quota.example.org"`),
				unverifiedAddition("TestResource", "c", "ns-a", DryRunSkipWebhookUnavailable, `failed calling webhook "policy.example.org"`),
			)},
			want: []OutputWarning{
				{Message: webhookSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": `failed calling webhook "policy.example.org"`, "count": "2"}},
				{Message: webhookSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": `failed calling webhook "quota.example.org"`, "count": "1"}},
			},
		},
		"MissingNamespaceAcrossInputsCountsEachResourceOnce": {
			reason: "Additions from several inputs share one warning. A resource two overlapping inputs both render is one resource, so it is counted once.",
			diffSets: []map[string]*ResourceDiff{
				diffMap(
					unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
					unverifiedAddition("TestResource", "shared", "ns-a", DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
				),
				diffMap(
					unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
					unverifiedAddition("TestResource", "shared", "ns-a", DryRunSkipNamespaceNotFound, `namespaces "ns-a" not found`),
				),
			},
			want: []OutputWarning{
				{Message: namespaceSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": `namespaces "ns-a" not found`, "count": "3"}},
			},
		},
		"ClusterScopedResourceKeepsAnEmptyNamespace": {
			reason: "A cluster-scoped addition still carries the namespace key, empty, so every summary has the same context keys.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "", DryRunSkipForbidden, "no create"),
			)},
			want: []OutputWarning{
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "", "reason": "no create", "count": "1"}},
			},
		},
		"ReasonsAreOrderedDeterministically": {
			reason: "Diffs arrive in map order, so the summary is sorted: by GVK, then namespace, then reason, then cause.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipWebhookUnavailable, "timeout"),
				unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipForbidden, "no create"),
				unverifiedAddition("TestResource", "c", "ns-a", DryRunSkipNamespaceNotFound, "gone"),
			)},
			want: []OutputWarning{
				{Message: forbiddenSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "reason": "no create", "count": "1"}},
				{Message: namespaceSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": "gone", "count": "1"}},
				{Message: webhookSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": "timeout", "count": "1"}},
			},
		},
		"DisabledIsNotWarnedAbout": {
			reason: "--dry-run-on=existing is the user's own choice, so the additions it leaves unverified are not news to them.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipDisabled, ""),
			)},
			want: nil,
		},
		"StorageVersionIsNotedOnTheGroupsWarning": {
			reason: "Additions requested at a version their CRD does not store say which version it stores, on the warning their group already raises, without splitting the group.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				storedAt(unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipForbidden, "no create"), "v1alpha1", "v1"),
				storedAt(unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipForbidden, "no create"), "v1alpha1", "v1"),
			)},
			want: []OutputWarning{
				{Message: forbiddenSummary + storageNote, Context: map[string]string{
					"gvk": "example.org/v1alpha1, Kind=TestResource", "namespace": "ns-a", "reason": "no create", "versions": "requested v1alpha1, stored v1", "count": "2",
				}},
			},
		},
		"StorageVersionNoteStaysWithItsOwnVersion": {
			reason: "Additions of one kind at the storage version and at another version are already two GVKs, so only the one at the other version carries the note.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				storedAt(unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipNamespaceNotFound, "gone"), "v1alpha1", "v1"),
				unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipNamespaceNotFound, "gone"),
			)},
			want: []OutputWarning{
				{Message: namespaceSummary, Context: map[string]string{"gvk": testResourceGVK, "namespace": "ns-a", "cause": "gone", "count": "1"}},
				{Message: namespaceSummary + storageNote, Context: map[string]string{
					"gvk": "example.org/v1alpha1, Kind=TestResource", "namespace": "ns-a", "cause": "gone", "versions": "requested v1alpha1, stored v1", "count": "1",
				}},
			},
		},
		"DisabledIsWarnedAboutForAStorageVersion": {
			reason: "--dry-run-on=existing is the user's choice, but that the cluster stores the additions at another version is not, so it is news worth a warning.",
			diffSets: []map[string]*ResourceDiff{diffMap(
				storedAt(unverifiedAddition("TestResource", "a", "ns-a", DryRunSkipDisabled, ""), "v1alpha1", "v1"),
				unverifiedAddition("TestResource", "b", "ns-a", DryRunSkipDisabled, ""),
			)},
			want: []OutputWarning{
				{Message: disabledSummary + storageNote, Context: map[string]string{
					"gvk": "example.org/v1alpha1, Kind=TestResource", "namespace": "ns-a", "versions": "requested v1alpha1, stored v1", "count": "1",
				}},
			},
		},
		"VerifiedDiffsRaiseNothing": {
			reason: "A diff without DryRunInfo was verified, and there is nothing to say about it.",
			diffSets: []map[string]*ResourceDiff{diffMap(&ResourceDiff{
				Gvk:          schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "TestResource"},
				Namespace:    "ns-a",
				ResourceName: "a",
				DiffType:     DiffTypeAdded,
			})},
			want: nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if d := cmp.Diff(tc.want, DryRunWarnings(tc.diffSets...)); d != "" {
				t.Errorf("\n%s\nDryRunWarnings() mismatch (-want +got):\n%s", tc.reason, d)
			}
		})
	}
}
