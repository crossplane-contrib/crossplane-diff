package testutils

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// crossplaneModule is the module whose CRDs the tests install. go.mod pins it, along with the xcrd that
// generates XR CRDs and the rest of Crossplane's code, so the CRDs match the code that consumes them.
const crossplaneModule = "github.com/crossplane/crossplane/v2"

// crossplaneCRDsDir resolves crossplaneModule once per test binary. Tests run in parallel and each asks for
// the directory, so a process-wide sync.OnceValues keeps the `go` invocation to one.
//
//nolint:gochecknoglobals // A once-per-process cache of an immutable lookup; it must be shared to be a cache.
var crossplaneCRDsDir = sync.OnceValues(func() (string, error) {
	// `go mod download -json <path>` with no version resolves the version go.mod's build list selects,
	// fetches it into the module cache only if it is missing, and reports where it lives.
	cmd := exec.Command("go", "mod", "download", "-json", crossplaneModule)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(err, "cannot resolve %s with `go mod download -json %s` (is the go command on PATH, "+
			"and the module reachable or already in the module cache?): %s", crossplaneModule, crossplaneModule,
			strings.TrimSpace(stderr.String()))
	}

	// The go command emits these keys capitalized (Version, Dir, Error); encoding/json matches keys
	// case-insensitively, so the repo's camelCase tags still decode them.
	var mod struct {
		Version string `json:"version"`
		Dir     string `json:"dir"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		return "", errors.Wrapf(err, "cannot parse `go mod download -json %s` output %q", crossplaneModule, out)
	}

	if mod.Error != "" {
		return "", errors.Errorf("cannot download %s: %s", crossplaneModule, mod.Error)
	}

	dir := filepath.Join(mod.Dir, "cluster", "crds")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", errors.Errorf("%s %s has no CRD directory at %s", crossplaneModule, mod.Version, dir)
	}

	return dir, nil
})

// CrossplaneCRDsDir returns the directory of Crossplane's own CRDs (Compositions, XRDs, Functions, …) in the
// github.com/crossplane/crossplane/v2 module at the version go.mod pins. It fails the test if the module
// cannot be resolved. The lookup runs once per test binary, so calling this from every test is cheap.
func CrossplaneCRDsDir(tb testing.TB) string {
	tb.Helper()

	dir, err := crossplaneCRDsDir()
	if err != nil {
		tb.Fatalf("cannot locate Crossplane's CRDs: %v", err)
	}

	return dir
}
