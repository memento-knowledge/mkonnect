package testca_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/memento-knowledge/mkonnect"

// No binary the project ships may link test-only code. The images are built from a context that
// excludes test files and internal/testca, so a production package that imported testca would fail to
// build there; one that imported net/http/httptest or testing would build, and pull test machinery
// into the connector.
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
