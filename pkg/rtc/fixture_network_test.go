package rtc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Prevent test fixtures from reverting to default OS networking, which
// launches a new firewall prompt for each temporary Windows test executable.
// Peers have to be created through the virtual-network helpers in rtctest.
func TestFixturesDoNotUseDefaultOSNetworking(t *testing.T) {
	var files []string
	for _, pattern := range []string{"*_test.go", filepath.Join("rtctest", "*.go")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) < 2 {
		t.Fatalf("expected to find the package's test files, got %v", files)
	}

	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "webrtc" && sel.Sel.Name == "NewPeerConnection" {
				t.Errorf("%s: fixture uses default OS network; use the virtual-network API", file)
			}
			return true
		})
	}
}
