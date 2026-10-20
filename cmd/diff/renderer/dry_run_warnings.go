package renderer

import (
	"fmt"
	"io"

	dt "github.com/crossplane-contrib/crossplane-diff/cmd/diff/result"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// writeWarnings writes each warning to w as its human-readable stderr line.
//
// Only warnings derived at render time belong here. Those raised during the run were written when they
// were raised (see diffprocessor.AdvisoryCollector), and writing them again would print them twice.
func writeWarnings(w io.Writer, warnings []dt.OutputWarning) error {
	for _, warning := range warnings {
		if _, err := fmt.Fprintln(w, warning.FormatWarning()); err != nil {
			return errors.Wrap(err, "failed to write warning to stderr")
		}
	}

	return nil
}
