package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSourceKnowsNothingOfTheWorkspace is the regression test for the rule that source code may know only APP_ENV:
// no ownership marker, worktree database name or database naming rule of any workspace tool may appear in it.
func TestSourceKnowsNothingOfTheWorkspace(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	forbidden := regexp.MustCompile(`workspaces-(project|tree)|speedtest_wt|\bwt_[a-z0-9]`)
	self := filepath.Join("internal", "center", "db", "hardcoded_names_test.go")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".tmp", ".worktrees", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".mjs") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == self {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if match := forbidden.Find(data); match != nil {
			t.Errorf("%s contains %q: source code must read only APP_ENV and the connection variables", rel, match)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
