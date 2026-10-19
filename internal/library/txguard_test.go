package library

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The writer transaction holds no network, filesystem, regex or worker wait.
// The guard parses the non-test files of internal/library and internal/store
// and rejects the packages that would let a Write closure wait on one, and in
// the library any goroutine or channel operation.

// forbiddenImport reports whether an import path is net, os, regexp, a regexp2
// module, sync, or the parser package, including their sub-packages.
func forbiddenImport(path string) bool {
	switch strings.SplitN(path, "/", 2)[0] {
	case "net", "os", "regexp", "sync":
		return true
	}
	return strings.Contains(path, "regexp2") ||
		strings.HasSuffix(path, "/internal/parse") || strings.Contains(path, "/internal/parse/")
}

// storeSyncUse is the store's existing use of sync outside any transaction:
// store.go guards the reader-pool resize with a Mutex and conn.go counts
// connects with atomics. Neither runs inside a Write closure. Any other file or
// any other sync import in the store fails the guard.
var storeSyncUse = map[string]string{"store.go": "sync", "conn.go": "sync/atomic"}

// guardViolations lists the rule breaks in one parsed file. concurrency also
// forbids goroutines and channel operations; exempt names one import to allow.
func guardViolations(fset *token.FileSet, f *ast.File, concurrency bool, exempt string) []string {
	var out []string
	at := func(p token.Pos, what string) {
		out = append(out, fset.Position(p).String()+": "+what)
	}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err == nil && path != exempt && forbiddenImport(path) {
			at(imp.Pos(), "imports "+path)
		}
	}
	if !concurrency {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			at(n.Pos(), "go statement")
		case *ast.ChanType:
			at(n.Pos(), "channel type")
		case *ast.SendStmt:
			at(n.Pos(), "channel send")
		case *ast.SelectStmt:
			at(n.Pos(), "select statement")
		case *ast.UnaryExpr:
			if n.Op == token.ARROW {
				at(n.Pos(), "channel receive")
			}
		}
		return true
	})
	return out
}

// scanDir applies the guard to every non-test .go file of dir and returns the file names scanned.
func scanDir(t *testing.T, dir string, concurrency bool, exempt map[string]string) (files, violations []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, name)
		violations = append(violations, guardViolations(fset, f, concurrency, exempt[name])...)
	}
	return files, violations
}

func TestWriterTxHoldsNoExternalWait(t *testing.T) {
	files, bad := scanDir(t, ".", true, nil)
	if !slices.Contains(files, "acquisition.go") || !slices.Contains(files, "review.go") {
		t.Fatalf("library files scanned = %v, want acquisition.go and review.go among them", files)
	}
	if len(bad) != 0 {
		t.Errorf("internal/library production code breaks the writer-transaction rule:\n%s", strings.Join(bad, "\n"))
	}

	files, bad = scanDir(t, filepath.Join("..", "store"), false, storeSyncUse)
	if !slices.Contains(files, "store.go") || !slices.Contains(files, "conn.go") {
		t.Fatalf("store files scanned = %v, want store.go and conn.go among them", files)
	}
	if len(bad) != 0 {
		t.Errorf("internal/store production code breaks the writer-transaction rule:\n%s", strings.Join(bad, "\n"))
	}
}

// The guard itself: each forbidden construct is found, clean code is not.
func TestWriterTxGuardDetects(t *testing.T) {
	check := func(src string, concurrency bool, want int) {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("parse: %v\n%s", err, src)
		}
		if got := guardViolations(fset, f, concurrency, ""); len(got) != want {
			t.Errorf("violations = %v, want %d for:\n%s", got, want, src)
		}
	}
	for _, imp := range []string{"net", "net/http", "os", "os/exec", "regexp", "sync", "sync/atomic",
		"github.com/dlclark/regexp2/v2", "github.com/ViktorsBaikers/Boxset/internal/parse"} {
		check("package x\nimport _ \""+imp+"\"\n", true, 1)
		check("package x\nimport _ \""+imp+"\"\n", false, 1)
	}
	for _, imp := range []string{"context", "database/sql", "path", "strings", "encoding/json", "github.com/ViktorsBaikers/Boxset/internal/store"} {
		check("package x\nimport _ \""+imp+"\"\n", true, 0)
	}
	check("package x\nfunc f(c int) { go f(c) }\n", true, 1)
	check("package x\nvar c chan int\n", true, 1)
	check("package x\nfunc f(c chan<- int) { c <- 1 }\n", true, 2)
	check("package x\nfunc f(c <-chan int) int { return <-c }\n", true, 2)
	check("package x\nfunc f(c <-chan int) { select { case <-c: } }\n", true, 3)
	check("package x\nfunc f(c int) { go f(c) }\n", false, 0) // the store is checked for imports only
	check("package x\nfunc f(a, b int) int { return a + b }\n", true, 0)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", "package x\nimport _ \"sync\"\nimport _ \"os\"\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := guardViolations(fset, f, false, "sync"); len(got) != 1 || !strings.Contains(got[0], "os") {
		t.Errorf("an exemption must cover only the named import: %v", got)
	}
}
