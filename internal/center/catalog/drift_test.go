package catalog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const root = "../../.."

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func equalNames(t *testing.T, label string, got, want []string) {
	t.Helper()
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %q, want %q", label, got, want)
	}
}

func captures(source, pattern string) []string {
	var out []string
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(source, -1) {
		out = append(out, m[1])
	}
	return out
}

func sqlNames(t *testing.T, source, pattern string) []string {
	t.Helper()
	match := regexp.MustCompile(pattern).FindStringSubmatch(source)
	if len(match) != 2 {
		t.Fatalf("SQL constraint not found: %s", pattern)
	}
	return captures(match[1], `'([^']+)'`)
}

func contractNames() (channels, metrics, operators, uiMetrics, uiOperators []string) {
	for _, c := range Channels() {
		channels = append(channels, c.Name)
	}
	for _, m := range Metrics() {
		metrics = append(metrics, m.Name)
		if !m.UIOmitted {
			uiMetrics = append(uiMetrics, m.Name)
		}
	}
	for _, op := range Operators() {
		operators = append(operators, op.Name)
		if !op.UIOmitted {
			uiOperators = append(uiOperators, op.Name)
		}
	}
	return
}

func TestSQLAndOpenAPIContractDrift(t *testing.T) {
	channels, metrics, operators, _, _ := contractNames()
	sql := readSource(t, "migrations/00001_initial.sql")
	equalNames(t, "SQL alert metrics", sqlNames(t, sql, `CHECK \(metric IN \(([^)]+)\)`), metrics)
	equalNames(t, "SQL alert operators", sqlNames(t, sql, `CHECK \(op IN \(([^)]+)\)`), operators)
	equalNames(t, "SQL alert channels", sqlNames(t, sql, `channels <@ ARRAY\[([^]]+)\]`), channels)
	deliveries := readSource(t, "migrations/00006_notification_deliveries.sql")
	equalNames(t, "SQL delivery channels", sqlNames(t, deliveries, `CHECK \(channel IN \(([^)]+)\)`), channels)
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Enum       []string `yaml:"enum"`
				Properties map[string]struct {
					Enum  []string `yaml:"enum"`
					Ref   string   `yaml:"$ref"`
					Items struct {
						Ref string `yaml:"$ref"`
					} `yaml:"items"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(readSource(t, "docs/openapi.yaml")), &spec); err != nil {
		t.Fatal(err)
	}
	equalNames(t, "OpenAPI channels", spec.Components.Schemas["Channel"].Enum, channels)
	for _, name := range []string{"AlertRule", "AlertRuleInput"} {
		props := spec.Components.Schemas[name].Properties
		equalNames(t, name+" metrics", props["metric"].Enum, metrics)
		equalNames(t, name+" operators", props["op"].Enum, operators)
		if props["channels"].Items.Ref != "#/components/schemas/Channel" {
			t.Errorf("%s channels no longer reference catalog Channel", name)
		}
	}
}

func TestUIDeclaredSubsetDrift(t *testing.T) {
	channels, _, _, metrics, operators := contractNames()
	source := readSource(t, "ui/assets/js/pages/alerts.js")
	start := strings.Index(source, "var METRICS = [")
	end := strings.Index(source, "var SEV =")
	if start < 0 || end < start {
		t.Fatal("UI METRICS declaration missing")
	}
	source = source[start:end]
	equalNames(t, "UI metrics (remaining catalog metrics must be explicitly omitted)", captures(source, `value: '([^']+)'`), metrics)
	var ops []string
	for _, list := range captures(source, `ops: \[([^]]*)\]`) {
		ops = append(ops, captures(list, `'([^']+)'`)...)
	}
	slices.Sort(ops)
	ops = slices.Compact(ops)
	equalNames(t, "UI operators (remaining catalog operators must be explicitly omitted)", ops, operators)
	settings := readSource(t, "ui/assets/js/pages/settings.js")
	block := regexp.MustCompile(`(?s)var channels = \[(.*?)\];`).FindStringSubmatch(settings)
	if len(block) != 2 {
		t.Fatal("UI settings channel declaration missing")
	}
	equalNames(t, "UI settings channels", captures(block[1], `key: '([^']+)'`), channels)
	demo := regexp.MustCompile(`var ch = \[(.*)\];`).FindStringSubmatch(settings)
	if len(demo) != 2 {
		t.Fatal("UI demo channels missing")
	}
	equalNames(t, "UI demo channels", captures(demo[1], `\['([^']+)'`), channels)
}

func TestFunctionLength(t *testing.T) {
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(filepath.ToSlash(path), "/center/server/") {
			return nil
		}
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			lines := files.Position(fn.End()).Line - files.Position(fn.Pos()).Line + 1
			if lines > 60 {
				t.Errorf("%s:%d %s is %d lines (maximum 60)", path, files.Position(fn.Pos()).Line, fn.Name.Name, lines)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
