package testutils

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	clicrd "github.com/crossplane/cli/v2/cmd/crossplane/common/crd"
	clixr "github.com/crossplane/cli/v2/pkg/xr"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	un "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	xpextv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
)

// CRDsForXRD returns the CRDs Crossplane generates from an XRD manifest: the composite resource's CRD and,
// when the XRD sets spec.claimNames, the claim's. Generation is upstream's (crossplane-runtime pkg/xcrd, via
// the crossplane CLI's ConvertToCRDs), so a test cluster gets the same CRDs a real one would.
//
// xcrd consumes the XRD as Crossplane's controller reads it: at apiextensions.crossplane.io/v1, with the
// apiserver's defaults applied. A manifest has neither, and the defaults decide what is generated (the
// policy defaults the CRDs declare, and the scope of an XRD that omits it, which is Namespaced at v2 but
// LegacyCluster at v1). So the manifest is first defaulted exactly as the apiserver would: with the schema
// of its own version, then relabelled to v1 (the XRD CRD converts with strategy None, so only apiVersion
// changes) and defaulted with v1's. Both schemas come from xrdCRD, the CompositeResourceDefinition CRD.
//
// The generated CRDs carry no owner reference: xcrd points one at the XRD, but the XRD has not been
// created yet when a test installs its CRDs, so it has no UID and an apiserver would reject the reference.
//
// The input is not modified.
func CRDsForXRD(xrd *un.Unstructured, xrdCRD *extv1.CustomResourceDefinition) ([]*extv1.CustomResourceDefinition, error) {
	if gk := xrd.GroupVersionKind().GroupKind(); gk != xpextv1.CompositeResourceDefinitionGroupVersionKind.GroupKind() {
		return nil, errors.Errorf("%s %q is not a CompositeResourceDefinition", gk, xrd.GetName())
	}

	defaulted := xrd.DeepCopy()

	if err := clixr.ApplyCRDDefaults(defaulted.Object, defaulted.GetAPIVersion(), *xrdCRD); err != nil {
		return nil, errors.Wrapf(err, "cannot default XRD %q at %s", xrd.GetName(), xrd.GetAPIVersion())
	}

	read := xpextv1.CompositeResourceDefinitionGroupVersionKind.GroupVersion().String()
	if defaulted.GetAPIVersion() != read {
		defaulted.SetAPIVersion(read)

		if err := clixr.ApplyCRDDefaults(defaulted.Object, read, *xrdCRD); err != nil {
			return nil, errors.Wrapf(err, "cannot default XRD %q at %s", xrd.GetName(), read)
		}
	}

	crds, err := clicrd.ConvertToCRDs([]*un.Unstructured{defaulted})
	if err != nil {
		return nil, errors.Wrapf(err, "cannot generate CRDs for XRD %q", xrd.GetName())
	}

	for _, crd := range crds {
		crd.SetOwnerReferences(nil)
	}

	return crds, nil
}

// pinnedCrossplaneCRDsTarget is the Earthly target that writes the directory PinnedCrossplaneCRDsDir returns.
const pinnedCrossplaneCRDsTarget = "earthly +fetch-crossplane-crds-gomod"

// PinnedCrossplaneCRDsDir returns the directory of Crossplane's own CRDs (Compositions, XRDs, Functions, …) at the
// version of github.com/crossplane/crossplane/v2 that go.mod pins, so they match the xcrd and the rest of the
// Crossplane code under test. `earthly +fetch-crossplane-crds-gomod` writes them to cluster/gomod/crds at the
// repository root. They are located from this source file rather than the working directory, and the test fails
// with the target to run if they are missing.
func PinnedCrossplaneCRDsDir(tb testing.TB) string {
	tb.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("cannot locate Crossplane's CRDs: runtime.Caller gave no file for testutils")
	}

	// This file is cmd/diff/testutils/xrd_crds.go, three levels below the repository root.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "cluster", "gomod", "crds")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		tb.Fatalf("Crossplane's CRDs are not at %s; run `%s` to fetch the version go.mod pins",
			dir, pinnedCrossplaneCRDsTarget)
	}

	return dir
}
