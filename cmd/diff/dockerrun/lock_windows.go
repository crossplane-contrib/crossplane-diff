//go:build windows

/*
Copyright 2026 The Crossplane Authors.

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

package dockerrun

import (
	"os"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// lock is unsupported on Windows, where a file that is open cannot be renamed or removed, which the
// lease protocol relies on. Without a lease a run's resources are never reclaimed by another run; the
// run still removes its own when it exits.
func lock(_ *os.File) error {
	return errors.New("run leases are not supported on Windows")
}
