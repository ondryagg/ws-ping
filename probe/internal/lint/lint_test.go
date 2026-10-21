// Package lint holds source-level guard tests. The spec asks for a rule that
// catches unbounded connection I/O; these tests are that rule.
package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseDir(t *testing.T, dir string, skip func(name string) bool) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if skip != nil && skip(p) {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		out[p] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// isSupport reports files that are test support or this package itself.
func isSupport(p string) bool {
	for _, d := range []string{"/internal/lint/", "/internal/testutil/", "/internal/testcert/", "/test/"} {
		if strings.Contains(filepath.ToSlash(p), d) {
			return true
		}
	}
	return false
}

func containsCall(fn *ast.FuncDecl, pkg, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == name {
				if pkg == "" {
					found = true
				} else if id, ok := s.X.(*ast.Ident); ok && id.Name == pkg {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// Every WebSocket read and write in the session code must go through
// readFrame or writeFrame, and those must set a net deadline and a context
// deadline. Anything else calling .Read/.Write/.Reader/.Writer on a
// connection in these files fails the build.
func TestWebSocketIOIsAlwaysBounded(t *testing.T) {
	files := parseDir(t, "../server", func(p string) bool { return filepath.Base(p) != "ws.go" })
	if len(files) != 1 {
		t.Fatal("ws.go not found")
	}
	for path, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			io := false
			ast.Inspect(fn, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if s, ok := c.Fun.(*ast.SelectorExpr); ok {
						switch s.Sel.Name {
						case "Read", "Write", "Reader", "Writer":
							io = true
						}
					}
				}
				return true
			})
			if !io {
				continue
			}
			switch fn.Name.Name {
			case "readFrame":
				if !containsCall(fn, "", "SetReadDeadline") || !containsCall(fn, "context", "WithDeadline") {
					t.Errorf("%s: readFrame must set a read deadline and a context deadline", path)
				}
			case "writeFrame":
				if !containsCall(fn, "", "SetWriteDeadline") || !containsCall(fn, "context", "WithDeadline") {
					t.Errorf("%s: writeFrame must set a write deadline and a context deadline", path)
				}
			default:
				t.Errorf("%s: %s performs connection I/O outside readFrame/writeFrame", path, fn.Name.Name)
			}
		}
	}
}

// The close handshake must run under a deadline, because the library's own
// close wait is a fixed 5 s.
func TestCloseHandshakeIsBounded(t *testing.T) {
	for path, f := range parseDir(t, "../server", nil) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if containsCall(fn, "", "Close") && fn.Name.Name == "closeWith" && !containsCall(fn, "", "SetDeadline") {
				t.Errorf("%s: closeWith must set a deadline before Close", path)
			}
		}
	}
}

// Production code must not use helpers that carry no timeouts.
func TestNoUnboundedHTTPHelpers(t *testing.T) {
	banned := map[string]bool{"ListenAndServe": true, "ListenAndServeTLS": true, "Get": true, "Post": true, "Head": true}
	for path, f := range parseDir(t, "../..", isSupport) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if s, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := s.X.(*ast.Ident); ok && id.Name == "http" && banned[s.Sel.Name] {
						t.Errorf("%s: http.%s has no timeouts; use a configured server or client", path, s.Sel.Name)
					}
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "http" && x.Sel.Name == "DefaultClient" {
					t.Errorf("%s: http.DefaultClient has no timeout", path)
				}
			case *ast.CompositeLit:
				if s, ok := x.Type.(*ast.SelectorExpr); ok && s.Sel.Name == "Server" {
					if id, ok := s.X.(*ast.Ident); ok && id.Name == "http" {
						hasHeaderTimeout := false
						for _, e := range x.Elts {
							if kv, ok := e.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ReadHeaderTimeout" {
									hasHeaderTimeout = true
								}
							}
						}
						if !hasHeaderTimeout {
							t.Errorf("%s: http.Server literal without ReadHeaderTimeout", path)
						}
					}
				}
			}
			return true
		})
	}
}

// context.Background() is only legitimate at the roots of the process.
func TestContextBackgroundOnlyAtRoots(t *testing.T) {
	allowed := map[string]bool{
		"New": true, "Shutdown": true, // server: base context; app: shutdown timeouts
		"Run": true, "main": true, "run": true, "Healthcheck": true, // process entry points
	}
	for path, f := range parseDir(t, "../..", isSupport) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || allowed[fn.Name.Name] {
				continue
			}
			if containsCall(fn, "context", "Background") || containsCall(fn, "context", "TODO") {
				t.Errorf("%s: %s uses context.Background/TODO", path, fn.Name.Name)
			}
		}
	}
}
