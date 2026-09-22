// internal/creds/store_internal_test.go
package creds

import (
	"os"
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
