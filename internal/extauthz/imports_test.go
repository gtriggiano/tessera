package extauthz

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// envoyModule is the module of Envoy's types.
const envoyModule = "github.com/envoyproxy/"

// TestOnlyTheAdapterImportsEnvoy keeps Envoy's types out of every package but
// this one, so that the engine never learns about Envoy. Test files may
// import them, to play Envoy's part.
func TestOnlyTheAdapterImportsEnvoy(t *testing.T) {
	root := filepath.Join("..", "..")
	adapter := filepath.Join(root, "internal", "extauthz")
	checked := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			if filepath.Dir(path) == adapter {
				return nil
			}
			for _, spec := range file.Imports {
				if p, _ := strconv.Unquote(spec.Path.Value); strings.HasPrefix(p, envoyModule) {
					t.Errorf("%s imports %s: only internal/extauthz may import Envoy's types", path, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked == 0 {
		t.Fatal("no Go file checked")
	}
}
