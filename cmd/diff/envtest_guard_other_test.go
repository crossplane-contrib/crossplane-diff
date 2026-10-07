//go:build !darwin && !linux

package main

// listProcesses lists no processes: the integration tests run envtest only on darwin and linux, so there is nothing to
// guard elsewhere.
func listProcesses(func(ppid int, name string) bool) ([]process, error) {
	return nil, nil
}
