package testca_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/memento-knowledge/mkonnect"

// No binary the project ships may link test-only code. The images are built from a context that
// excludes test files, so a production package that imported testca (or net/http/httptest) would
// either fail to build there or, worse, pull a throwaway-CA generator into the connector.
func TestProductionBinariesDoNotDependOnTestOnlyPackages(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool not available: %v", err)
	}
	cmd := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/...")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list reported no packages")
	}
	for _, pkg := range deps {
		inModule := pkg == modulePath || strings.HasPrefix(pkg, modulePath+"/")
		if (inModule && strings.HasSuffix(pkg, "/internal/testca")) || pkg == "net/http/httptest" || pkg == "testing" {
			t.Errorf("a shipped binary depends on the test-only package %s", pkg)
		}
	}
}
