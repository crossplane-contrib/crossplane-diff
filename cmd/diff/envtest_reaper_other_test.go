//go:build !darwin && !linux

package main

// listOrphanCandidates lists nothing: the integration tests run envtest only on darwin and linux.
func listOrphanCandidates() ([]process, error) {
	return nil, nil
}
