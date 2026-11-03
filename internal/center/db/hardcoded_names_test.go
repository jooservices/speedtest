package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoWorktreeDatabaseNameIsHardCoded is the regression test for the recurring mistake of pinning an integration
// test to one worktree's database (for example speedtest_wt23), which breaks the suite in every other worktree.
// Tests must use CheckTestDatabase instead.
func TestNoWorktreeDatabaseNameIsHardCoded(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	pattern := regexp.MustCompile(`speedtest_wt[0-9a-z]+`)
	allowed := map[string]bool{
		filepath.Join("internal", "center", "db", "test_database_test.go"):   true, // table of accepted and refused names
		filepath.Join("cmd", "center", "main_test.go"):                       true, // unit test of the local database name rule
		filepath.Join("internal", "center", "db", "hardcoded_names_test.go"): true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".tmp", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if match := pattern.Find(data); match != nil {
			t.Errorf("%s hard-codes the database name %q; use db.CheckTestDatabase", rel, match)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
