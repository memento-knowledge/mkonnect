package gitsync

import "testing"

// The provider gate must open only for a failure that says the credential itself was refused. A
// credential that is valid but cannot see one repository (a 403, or the 404 forges answer for a
// repository they hide) is that repository's problem, and must not hold back its siblings.
func TestOriginCredentialRejected(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"http 401", "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 401", true},
		{"authentication failed", "fatal: Authentication failed for 'https://git.example.com/repo-a/'", true},
		{"no credential supplied", "fatal: could not read Username for 'https://git.example.com': terminal prompts disabled", true},
		{"basic access denied", "remote: HTTP Basic: Access denied\nfatal: Authentication failed for 'https://git.example.com/repo-a/'", true},
		{"http 403 is a repository permission", "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 403", false},
		{"403 forbidden wording", "fatal: unable to update url base from redirection: HTTP/1.1 403 Forbidden", false},
		{"http 404", "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 404", false},
		{"repository not found", "fatal: repository 'https://git.example.com/repo-a/' not found", false},
		{"unreachable", "fatal: unable to access 'https://git.example.com/repo-a/': Could not resolve host: git.example.com", false},
		{"proxy auth", "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 407", false},
		// The remote's own words are not evidence about the local credential's standing.
		{"only the remote says so", "remote: Authentication failed\nfatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 500", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := originCredentialRejected([]byte(tt.stderr)); got != tt.want {
				t.Fatalf("originCredentialRejected(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}
