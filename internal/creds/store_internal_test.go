// internal/creds/store_internal_test.go
package creds

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadOnlyModeAcceptable pins the permission policy used when the credentials
// file lives on a read-only mount and cannot be chmod-ed to 0600. Group read must
// be allowed (a non-root connector reads a root-owned Kubernetes Secret through
// the fsGroup bit), while world access and group write must be rejected.
func TestReadOnlyModeAcceptable(t *testing.T) {
	tests := []struct {
		mode os.FileMode
		want bool
	}{
		{0o600, true},  // owner rw only
		{0o400, true},  // owner read only
		{0o640, true},  // owner rw, group read — the Kubernetes Secret case
		{0o440, true},  // owner+group read — chart default (defaultMode 0440)
		{0o750, true},  // group execute is allowed (confidentiality is unaffected)
		{0o660, false}, // group write is not allowed
		{0o644, false}, // world read is not allowed
		{0o604, false}, // world read is not allowed
		{0o444, false}, // world read is not allowed
		{0o777, false}, // wide open
	}
	for _, tt := range tests {
		if got := readOnlyModeAcceptable(tt.mode); got != tt.want {
			t.Errorf("readOnlyModeAcceptable(%#o) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

// TestNewReadOnlyMountBranch exercises the path taken when the credentials file
// cannot be chmod-ed to 0600 (a read-only mount such as a Kubernetes Secret). We
// cannot make chmod fail portably as the file's owner, so we stub the package
// chmod var to force the fallback and assert it accepts only safe modes.
func TestNewReadOnlyMountBranch(t *testing.T) {
	orig := chmod
	chmod = func(string, os.FileMode) error { return errors.New("read-only file system") }
	t.Cleanup(func() { chmod = orig })

	valid, err := json.Marshal(map[string]Credential{
		"jenkins": {BaseURL: "http://jenkins:8080", Auth: "bearer", Token: "test-token-value-123"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	tests := []struct {
		name       string
		mode       os.FileMode
		wantAccept bool
	}{
		{"group-read 0640 (secret case)", 0o640, true},
		{"owner+group read 0440", 0o440, true},
		{"world-read 0644", 0o644, false},
		{"group-write 0660", 0o660, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, valid, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Set the exact bits after writing so umask does not interfere.
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatalf("chmod fixture: %v", err)
			}

			_, err := New(path)
			if tt.wantAccept && err != nil {
				t.Fatalf("New(%#o) on read-only mount: want accept, got %v", tt.mode, err)
			}
			if !tt.wantAccept {
				if err == nil {
					t.Fatalf("New(%#o) on read-only mount: want rejection, got nil", tt.mode)
				}
				// The rejection must not echo the file contents (which hold the token).
				if got := err.Error(); strings.Contains(got, "test-token-value-123") {
					t.Fatalf("error leaked credential contents: %q", got)
				}
			}
		})
	}
}
