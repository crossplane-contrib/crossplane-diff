package testutils

import (
	stdlog "log"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/go-logr/stdr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kt "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// Colors for terminal output.
const (
	ColorRed    = "\x1b[31m"
	ColorGreen  = "\x1b[32m"
	ColorYellow = "\x1b[33m"
	ColorReset  = "\x1b[0m"
)

// color takes a multiline string, splits it by line, and adds the specified coloring to each line.
// It returns a single string with all lines joined back together.
func color(colorCode, input string) string {
	lines := strings.Split(input, "\n")
	coloredLines := make([]string, 0, len(lines))

	for _, line := range lines {
		// Handle the case of the last empty line after a newline
		if line == "" && len(coloredLines) == len(lines)-1 {
			coloredLines = append(coloredLines, "")
			continue
		}

		coloredLines = append(coloredLines, colorCode+line+ColorReset)
	}

	return strings.Join(coloredLines, "\n")
}

// Green takes a multiline string, splits it by line, and adds green coloring to each line.
// It returns a single string with all lines joined back together.
func Green(input string) string {
	return color(ColorGreen, input)
}

// Red takes a multiline string, splits it by line, and adds red coloring to each line.
// It returns a single string with all lines joined back together.
func Red(input string) string {
	return color(ColorRed, input)
}

// Yellow takes a multiline string, splits it by line, and adds yellow coloring to each line.
// It returns a single string with all lines joined back together.
func Yellow(input string) string {
	return color(ColorYellow, input)
}

// CompareIgnoringAnsi compares two strings after stripping ANSI special characters.
func CompareIgnoringAnsi(expected, actual string) bool {
	// Strip ANSI codes from both strings
	ansiPattern := regexp.MustCompile("\x1b\\[[0-9;]*m")
	expectedStripped := ansiPattern.ReplaceAllString(expected, "")
	actualStripped := ansiPattern.ReplaceAllString(actual, "")

	// Compare the stripped strings
	return expectedStripped == actualStripped
}

// SetupKubeTestLogger sets the global logger for use of the Kube environment to the T.Log of this test.
func SetupKubeTestLogger(t *testing.T) {
	t.Helper()

	// Create a logr.Logger that writes to testing.T.Log
	testLogger := stdr.NewWithOptions(stdlog.New(testWriter{t}, "", 0), stdr.Options{LogCaller: stdr.All})

	// Set the logger for controller-runtime
	log.SetLogger(testLogger)
}

// testWriter adapts testing.T.Log to io.Writer.
type testWriter struct {
	t *testing.T
}

// Write logs the provided argument as a string to the T.Log.
func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Log(string(p))
	return len(p), nil
}

// TestLogger coerces the T.Log into the shape of a Logr logger.
func TestLogger(t *testing.T, verbose bool) logging.Logger {
	verbosity := 0
	if verbose {
		verbosity = 1
	}

	return logging.NewLogrLogger(testr.NewWithOptions(t, testr.Options{Verbosity: verbosity}))
}

// AdvisoryCapturingLogger wraps a logging.Logger and records the messages passed to Info, so a test
// can assert that a call site raises a user-facing advisory rather than mere tracing.
//
// Info is the codebase's advisory level: the CLI wraps the injected logger so every Info becomes a
// stderr WARNING line and a structured warnings[] entry. Emitters deliberately know nothing about
// that wrapper, so tests for them assert the contract they actually have — "this calls Info, not
// Debug" — instead of coupling to the wrapper. That also lets packages the wrapper's own package
// imports (the clients) assert their advisories without an import cycle.
type AdvisoryCapturingLogger struct {
	wrapped logging.Logger

	mu       sync.Mutex
	advisory []string
}

// NewAdvisoryCapturingLogger returns a logger that forwards to a standard test logger while recording
// Info messages for assertion.
func NewAdvisoryCapturingLogger(t *testing.T) *AdvisoryCapturingLogger {
	t.Helper()

	return &AdvisoryCapturingLogger{wrapped: TestLogger(t, false)}
}

// Info records msg as an advisory and forwards to the wrapped logger.
func (l *AdvisoryCapturingLogger) Info(msg string, keysAndValues ...any) {
	l.mu.Lock()
	l.advisory = append(l.advisory, msg)
	l.mu.Unlock()

	l.wrapped.Info(msg, keysAndValues...)
}

// Debug forwards to the wrapped logger without recording: tracing is not an advisory.
func (l *AdvisoryCapturingLogger) Debug(msg string, keysAndValues ...any) {
	l.wrapped.Debug(msg, keysAndValues...)
}

// WithValues returns a derived logger that records into the same place, so an advisory raised through
// a derived logger is still captured.
func (l *AdvisoryCapturingLogger) WithValues(keysAndValues ...any) logging.Logger {
	return &advisoryChild{parent: l, wrapped: l.wrapped.WithValues(keysAndValues...)}
}

// Advisories returns the Info messages recorded so far, in call order.
func (l *AdvisoryCapturingLogger) Advisories() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string{}, l.advisory...)
}

// advisoryChild is a WithValues-derived logger that records into its parent.
type advisoryChild struct {
	parent  *AdvisoryCapturingLogger
	wrapped logging.Logger
}

func (l *advisoryChild) Info(msg string, keysAndValues ...any) {
	l.parent.mu.Lock()
	l.parent.advisory = append(l.parent.advisory, msg)
	l.parent.mu.Unlock()

	l.wrapped.Info(msg, keysAndValues...)
}

func (l *advisoryChild) Debug(msg string, keysAndValues ...any) {
	l.wrapped.Debug(msg, keysAndValues...)
}

func (l *advisoryChild) WithValues(keysAndValues ...any) logging.Logger {
	return &advisoryChild{parent: l.parent, wrapped: l.wrapped.WithValues(keysAndValues...)}
}

// CreateFakeDiscoveryClient is a helper function to create a fake discovery client for testing.
// The fake discovery client automatically derives API groups from the provided resources,
// enabling both ServerResourcesForGroupVersion() and ServerGroups() to work correctly.
func CreateFakeDiscoveryClient(resources map[string][]metav1.APIResource) discovery.DiscoveryInterface {
	fakeDiscovery := &fakediscovery.FakeDiscovery{
		Fake: &kt.Fake{},
	}

	apiResourceLists := make([]*metav1.APIResourceList, 0, len(resources))

	for gv, apiResources := range resources {
		apiResourceLists = append(apiResourceLists, &metav1.APIResourceList{
			GroupVersion: gv,
			APIResources: apiResources,
		})
	}

	fakeDiscovery.Resources = apiResourceLists
	fakeDiscovery.Resources = apiResourceLists

	return fakeDiscovery
}
