// Package suite runs the tests of a conformance suite under the names conformance.md gives them, and
// reports the pending ones by name, so a test that did not run is never read as one that passed.
package suite

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Test is one test of conformance.md, by its name there.
type Test struct {
	Name string
	Run  func(t *testing.T)
}

var (
	mu      sync.Mutex
	pending []string
)

// Pending ends t as pending: a test whose implementation cannot pass yet, reported by name.
func Pending(t *testing.T, reason string) {
	t.Helper()
	mu.Lock()
	pending = append(pending, t.Name()+": "+reason)
	mu.Unlock()
	t.Skip("pending: " + reason)
}

// Run runs each test as a subtest of t under its conformance.md name, less the suite's prefix, in the
// order given.
func Run(t *testing.T, prefix string, tests []Test) {
	for _, test := range tests {
		t.Run(strings.TrimPrefix(test.Name, prefix), test.Run)
	}
}

// Main runs the suite's tests and then writes the pending ones to stdout, by name.
func Main(m *testing.M) int {
	code := m.Run()
	summarize(os.Stdout)
	return code
}

func summarize(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(pending)
	_, _ = fmt.Fprintf(w, "conformance: %d pending, not run and not passed\n", len(pending))
	for _, p := range pending {
		_, _ = fmt.Fprintf(w, "  pending %s\n", p)
	}
}

// Names are the names of conformance.md's tests whose name begins with prefix, read from the file at
// path.
func Names(path, prefix string) ([]string, error) {
	read, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(read), "\n") {
		if name, ok := strings.CutPrefix(line, "| `"+prefix); ok {
			if end := strings.Index(name, "`"); end > 0 {
				out = append(out, prefix+name[:end])
			}
		}
	}
	return out, nil
}
