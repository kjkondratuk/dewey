package safety

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The README claims the always-read fast path applies "only for targets inside
// the project root". Glob's confinement check reads `path` only; `pattern` is
// never examined, and globFiles joins it onto the root afterwards.
// filepath.Join cleans "..", so the effective search root leaves the project
// with no classification at all.
func TestAlwaysRead_GlobPatternCannotEscapeTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	args := `{"pattern":"../../../../../../../etc/*.conf","path":` + quote(root) + `}`

	m := &countingModel{}
	c := NewLLMClassifier(m, testRules(t), root)
	got, err := c.Classify(context.Background(), "Glob", "Finds files by glob pattern", args)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if got.RequiresApproval() || m.calls != 0 {
		return // fixed: the escape is classified
	}
	t.Errorf("Glob with an escaping pattern got %+v (model calls = %d): the fast path "+
		"was granted for a search whose effective root is outside %s", got, m.calls, root)
}
