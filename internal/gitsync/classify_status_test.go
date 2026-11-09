package gitsync

import (
	"errors"
	"strings"
	"testing"
)

// httpFailure is git's stderr for a smart-HTTP request the server answered with an error status.
func httpFailure(status string) string {
	return "fatal: unable to access 'https://git.example.com/group/repository/': The requested URL returned error: " + status
}

// notFoundFailure is git's stderr for a 404 on a repository: it prints no status number.
const notFoundFailure = "fatal: repository 'https://git.example.com/group/repository/' not found"

// The same status means a different thing on each side of the sync, and the three statuses that have
// a type of their own are pinned on both: a redirect from the origin, a 429 or 404 from the replica.
func TestClassifyGitFailureStatusBySide(t *testing.T) {
	exit := errors.New("exit status 128")
	tests := []struct {
		name         string
		side         gitSide
		stderr       string
		tokenExpired bool
		wantType     string
		wantDetail   string
	}{
		{"origin 301", originSide, httpFailure("301"), false, ErrOriginURLRedirected, detailOriginURLRedirected},
		{"origin 302", originSide, httpFailure("302"), false, ErrOriginURLRedirected, detailOriginURLRedirected},
		{"origin 307", originSide, httpFailure("307"), false, ErrOriginURLRedirected, detailOriginURLRedirected},
		{"origin 308", originSide, httpFailure("308"), false, ErrOriginURLRedirected, detailOriginURLRedirected},
		{"origin 303 is not one of the redirects", originSide, httpFailure("303"), false, ErrUnclassified, "fatal: unable to access '': The requested URL returned error: 303"},
		{"origin 404 stays access denied", originSide, httpFailure("404"), false, ErrOriginAccessDenied, ""},
		{"origin repository not found stays access denied", originSide, notFoundFailure, false, ErrOriginAccessDenied, ""},
		{"origin 429 is not a replica type", originSide, httpFailure("429"), false, ErrUnclassified, "fatal: unable to access '': The requested URL returned error: 429"},
		{"origin 503", originSide, httpFailure("503"), false, ErrOriginUnreachable, ""},

		{"replica 301 has no type of its own", replicaSide, httpFailure("301"), false, ErrUnclassified, "fatal: unable to access '': The requested URL returned error: 301"},
		{"replica 404", replicaSide, httpFailure("404"), false, ErrReplicaRepositoryNotFound, detailReplicaRepositoryNotFound},
		{"replica repository not found", replicaSide, notFoundFailure, false, ErrReplicaRepositoryNotFound, detailReplicaRepositoryNotFound},
		{"replica 404 in an RPC failure", replicaSide, "error: RPC failed; HTTP 404 curl 22 The requested URL returned error: 404", false, ErrReplicaRepositoryNotFound, detailReplicaRepositoryNotFound},
		{"replica 429", replicaSide, httpFailure("429"), false, ErrReplicaRateLimited, detailReplicaRateLimited},
		{"replica 429 in an RPC failure", replicaSide, "error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429", false, ErrReplicaRateLimited, detailReplicaRateLimited},
		// The replica answers 503 when it cannot check a push token: its problem, not a rejected token.
		{"replica 503 stays unreachable", replicaSide, httpFailure("503"), false, ErrReplicaUnreachable, ""},
		{"replica 503 with an expired token stays unreachable", replicaSide, httpFailure("503"), true, ErrReplicaUnreachable, ""},
		// A 403 may also be a firewall in front of the replica; it cannot be told from a refused token.
		{"replica 403 stays credential rejected", replicaSide, httpFailure("403"), false, ErrCredentialRejected, ""},
		{"replica 401 stays credential rejected", replicaSide, httpFailure("401"), false, ErrCredentialRejected, ""},
		{"replica 403 with an expired token", replicaSide, httpFailure("403"), true, ErrPushTokenExpired, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typ, detail := classifyGitFailure(tt.side, exit, []byte(tt.stderr), tt.tokenExpired)
			if typ != tt.wantType || detail != tt.wantDetail {
				t.Fatalf("got (%q, %q), want (%q, %q)", typ, detail, tt.wantType, tt.wantDetail)
			}
		})
	}
}

// The types a status picks are picked from git's own lines. Text the far end chose ("remote:" lines,
// a rejection reason) can neither set the status nor spell the phrase, and cannot lose a real status to
// a keyword in its own text.
func TestClassifyGitFailureStatusIgnoresRemoteText(t *testing.T) {
	exit := errors.New("exit status 128")
	tests := []struct {
		name     string
		side     gitSide
		stderr   string
		wantType string
	}{
		{"origin remote line claims a redirect", originSide,
			"remote: The requested URL returned error: 301\nfatal: something unexpected went wrong", ErrUnclassified},
		{"origin remote line claims a redirect beside a 401", originSide,
			"remote: The requested URL returned error: 301\n" + httpFailure("401"), ErrOriginAccessDenied},
		{"replica remote line claims a 429", replicaSide,
			"remote: The requested URL returned error: 429\nfatal: something unexpected went wrong", ErrUnclassified},
		{"replica remote line claims a 404", replicaSide,
			"remote: The requested URL returned error: 404\nfatal: something unexpected went wrong", ErrUnclassified},
		{"replica remote line spells the phrase", replicaSide,
			"remote: fatal: repository 'x' not found\n ! [remote rejected] main -> main (hook declined)", ErrReplicaRejected},
		{"replica rejection reason spells the phrase", replicaSide,
			" ! [remote rejected] main -> main (fatal: repository 'x' not found)", ErrReplicaRejected},
		// Text the remote chose that git relays on a line of its own, or as a rejected ref's reason, is
		// not git's status either.
		{"replica rejection reason claims a 404", replicaSide,
			" ! [remote rejected] main -> main (upstream returned error: 404)", ErrReplicaRejected},
		{"replica rejection reason claims a 429", replicaSide,
			" ! [remote rejected] main -> main (The requested URL returned error: 429)", ErrReplicaRejected},
		{"replica relayed error claims a 429", replicaSide,
			"fatal: remote error: The requested URL returned error: 429", ErrUnclassified},
		{"replica unpack failure claims a 404", replicaSide,
			"error: remote unpack failed: The requested URL returned error: 404", ErrUnclassified},
		{"origin relayed error claims a redirect", originSide,
			"fatal: remote error: The requested URL returned error: 301", ErrUnclassified},
		// A real 429 whose own text mentions words other arms read: the status wins.
		{"replica 429 with quota and timeout words in a remote line", replicaSide,
			"remote: quota exceeded, too large a burst, timed out waiting for ssl\n" + httpFailure("429"), ErrReplicaRateLimited},
		{"replica 404 with quota words in a remote line", replicaSide,
			"remote: quota exceeded\n" + httpFailure("404"), ErrReplicaRepositoryNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if typ, _ := classifyGitFailure(tt.side, exit, []byte(tt.stderr), false); typ != tt.wantType {
				t.Fatalf("error_type = %q, want %q (stderr %q)", typ, tt.wantType, tt.stderr)
			}
		})
	}
}

// The detail of the three new types is a fixed hint. Whatever git or the server printed (the redirect
// target, a host, a path, a branch, a credential) must not reach it: the detail is the constant, and
// the constants themselves carry none of those.
func TestClassifyGitFailureHintsNeverCarryServerText(t *testing.T) {
	exit := errors.New("exit status 128")
	needles := []string{
		"other.example.net", "moved/location", "group/repository", "git.example.com", "replica.example.com",
		"secret-branch-name", "secret-credential-0123456789", "https://", "127.0.0.1", "/",
	}
	noise := "remote: Location: https://other.example.net/moved/location?token=secret-credential-0123456789\n" +
		" * [new branch]      secret-branch-name -> secret-branch-name\n"
	tests := []struct {
		name   string
		side   gitSide
		stderr string
		want   string
	}{
		{"origin redirect", originSide, noise + "fatal: unable to access 'https://git.example.com/group/repository/': The requested URL returned error: 308", detailOriginURLRedirected},
		{"replica rate limit", replicaSide, noise + "fatal: unable to access 'https://replica.example.com/group/repository/': The requested URL returned error: 429", detailReplicaRateLimited},
		{"replica not found", replicaSide, noise + "fatal: repository 'https://replica.example.com/group/repository/' not found", detailReplicaRepositoryNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, detail := classifyGitFailure(tt.side, exit, []byte(tt.stderr), false)
			if detail != tt.want {
				t.Fatalf("detail = %q, want the fixed hint %q", detail, tt.want)
			}
			for _, n := range needles {
				if strings.Contains(detail, n) {
					t.Errorf("detail %q carries %q", detail, n)
				}
			}
		})
	}
}

// The redirect hint is the wording the platform relays to operators; pin it exactly.
func TestRedirectHintWording(t *testing.T) {
	const want = "the origin redirected the repository URL: use the repository's canonical clone URL (renamed or moved?)"
	if detailOriginURLRedirected != want {
		t.Errorf("redirect hint = %q, want %q", detailOriginURLRedirected, want)
	}
}
