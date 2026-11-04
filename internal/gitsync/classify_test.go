package gitsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyGitFailure(t *testing.T) {
	exit := errors.New("exit status 128")
	timedOut := fmt.Errorf("push timed out: %w", context.DeadlineExceeded)

	tests := []struct {
		name         string
		side         gitSide
		err          error
		stderr       string
		tokenExpired bool
		want         string
	}{
		{"success", originSide, nil, "", false, ""},

		// Origin side.
		{"origin 401", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 401", false, ErrOriginAccessDenied},
		{"origin 403", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 403", false, ErrOriginAccessDenied},
		{"origin auth failed", originSide, exit, "fatal: Authentication failed for 'https://git.example.com/repo-a/'", false, ErrOriginAccessDenied},
		{"origin prompts disabled", originSide, exit, "fatal: could not read Username for 'https://git.example.com': terminal prompts disabled", false, ErrOriginAccessDenied},
		{"origin repo not found is access denied", originSide, exit, "fatal: repository 'https://git.example.com/repo-a/' not found", false, ErrOriginAccessDenied},
		{"origin 404 status is access denied", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 404", false, ErrOriginAccessDenied},
		{"origin dns", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': Could not resolve host: git.example.com", false, ErrOriginUnreachable},
		{"origin refused", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': Failed to connect to git.example.com port 443: Connection refused", false, ErrOriginUnreachable},
		{"origin tls", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': SSL certificate problem: unable to get local issuer certificate", false, ErrOriginUnreachable},
		{"origin 502", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 502", false, ErrOriginUnreachable},
		{"origin 407 proxy auth is not a credential rejection", originSide, exit, "fatal: unable to access 'https://git.example.com/repo-a/': The requested URL returned error: 407", false, ErrOriginUnreachable},
		{"origin deadline", originSide, fmt.Errorf("fetch timed out: %w", context.DeadlineExceeded), "", false, ErrOriginUnreachable},
		// A hostname containing a connection keyword must not flip the result: git echoes the URL.
		{"origin tls in hostname, 403", originSide, exit, "fatal: unable to access 'https://tls.example.com/repo-a/': The requested URL returned error: 403", false, ErrOriginAccessDenied},

		// Replica side.
		{"replica 401, token not expired", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': The requested URL returned error: 401", false, ErrCredentialRejected},
		{"replica 401, token expired", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': The requested URL returned error: 401", true, ErrPushTokenExpired},
		{"replica 403, token expired", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': The requested URL returned error: 403", true, ErrPushTokenExpired},
		{"replica auth failed", replicaSide, exit, "fatal: Authentication failed for 'https://replica.example.com/r/'", false, ErrCredentialRejected},
		{"replica dns", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': Could not resolve host: replica.example.com", false, ErrReplicaUnreachable},
		{"replica refused", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': Failed to connect to replica.example.com port 443: Connection refused", false, ErrReplicaUnreachable},
		{"replica tls", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': SSL certificate problem: self signed certificate", false, ErrReplicaUnreachable},
		{"replica 503", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': The requested URL returned error: 503", false, ErrReplicaUnreachable},
		{"replica size limit", replicaSide, exit, "remote: error: repository size limit exceeded\nTo https://replica.example.com/r.git\n ! [remote rejected] main -> main (pre-receive hook declined)", false, ErrReplicaRejected},
		{"replica pack too large", replicaSide, exit, "error: remote unpack failed: pack exceeds maximum allowed size", false, ErrReplicaRejected},
		{"replica 413", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': The requested URL returned error: 413", false, ErrReplicaRejected},
		{"replica quota", replicaSide, exit, "remote: error: storage quota exceeded for this repository", false, ErrReplicaRejected},
		{"replica payload too large", replicaSide, exit, "error: RPC failed; HTTP 413 curl 22 The requested URL returned error: 413", false, ErrReplicaRejected},
		{"replica hook declined", replicaSide, exit, "To https://replica.example.com/r.git\n ! [remote rejected] main -> main (pre-receive hook declined)", false, ErrReplicaRejected},
		{"replica deadline is push timeout", replicaSide, timedOut, "", false, ErrPushTimeout},

		// Local faults, either side.
		{"disk full on origin fetch", originSide, exit, "fatal: write error: No space left on device", false, ErrLocalDiskExhausted},
		{"disk full on push", replicaSide, exit, "error: unable to write file: No space left on device", false, ErrLocalDiskExhausted},
		// A "remote:" line is the replica speaking: its full disk is not ours.
		{"replica-side disk full is not local", replicaSide, exit, "remote: fatal: write error: No space left on device\n ! [remote rejected] main -> main (unpacker error)", false, ErrReplicaRejected},
		{"out of memory", originSide, exit, "fatal: Out of memory, malloc failed (tried to allocate 1073741824 bytes)", false, ErrJobOutOfMemory},
		{"killed with no deadline is oom", replicaSide, errors.New("signal: killed"), "", false, ErrJobOutOfMemory},
		// A child git process the kernel killed (git prints "<cmd> died of signal 9" and exits 128).
		{"index-pack killed by signal 9", originSide, exit, "error: index-pack died of signal 9", false, ErrJobOutOfMemory},
		{"pack-objects killed by signal 9", replicaSide, exit, "error: pack-objects died of signal 9\nfatal: the remote end hung up unexpectedly", false, ErrJobOutOfMemory},
		{"git-remote-https killed by signal 9", replicaSide, exit, "error: git-remote-https died of signal 9", false, ErrJobOutOfMemory},
		// Only signal 9 is the OOM killer: other signals, and numbers merely starting with 9, are not.
		{"index-pack killed by signal 15 is not oom", originSide, exit, "error: index-pack died of signal 15", false, ErrUnclassified},
		{"signal 90 is not oom", originSide, exit, "error: index-pack died of signal 90", false, ErrUnclassified},
		// A "remote:" line is the far end speaking: its child dying is not our memory.
		{"remote side signal 9 is not local oom", replicaSide, exit, "remote: error: index-pack died of signal 9\n ! [remote rejected] main -> main (unpacker error)", false, ErrReplicaRejected},
		// Server-supplied text that is not on a "remote:" line (a rejected-ref reason, a relayed error)
		// can name a dying child too; only git's own "<cmd> died of signal 9" line counts.
		{"signal 9 in a rejected-ref reason is not local oom", replicaSide, exit, " ! [remote rejected] main -> main (index-pack died of signal 9)", false, ErrReplicaRejected},
		{"signal 9 in a relayed unpack error is not local oom", replicaSide, exit, "error: remote unpack failed: index-pack died of signal 9\n ! [remote rejected] main -> main (unpacker error)", false, ErrReplicaRejected},
		{"signal 9 in a server error is not local oom", originSide, exit, "fatal: remote error: index-pack died of signal 9", false, ErrUnclassified},

		{"unrecognized origin", originSide, exit, "fatal: something unexpected went wrong", false, ErrUnclassified},
		{"unrecognized replica", replicaSide, exit, "fatal: something unexpected went wrong", false, ErrUnclassified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := classifyGitFailure(tt.side, tt.err, []byte(tt.stderr), tt.tokenExpired)
			if got != tt.want {
				t.Fatalf("error_type = %q, want %q (stderr %q)", got, tt.want, tt.stderr)
			}
			if tt.want == "" || tt.want == ErrUnclassified {
				return
			}
			if detail != "" {
				t.Errorf("a classified failure must carry no raw detail, got %q", detail)
			}
		})
	}
}

// git prints a status line per ref, so a branch or tag NAME must never steer the classification: a
// ref called "user-quota" is not a quota failure, and "tls-fix" is not a TLS failure.
func TestClassifyGitFailureIgnoresRefNames(t *testing.T) {
	exit := errors.New("exit status 128")
	push := "To https://replica.example.com/r.git\n" +
		" * [new branch]      feature/user-quota -> feature/user-quota\n" +
		" * [new tag]         ssl-timed-out-fix -> ssl-timed-out-fix\n" +
		" ! [remote rejected] main -> main (pre-receive hook declined)\n" +
		"error: failed to push some refs to 'https://replica.example.com/r.git'\n"
	if got, _ := classifyGitFailure(replicaSide, exit, []byte(push), false); got != ErrReplicaRejected {
		t.Errorf("a hook rejection beside quota/ssl-named refs = %q, want replica_rejected", got)
	}

	fetch := "From https://git.example.com/group/repo-a\n" +
		" * [new branch]      tls-fix -> tls-fix\n" +
		" * [new tag]         v1-corrupt-cache -> v1-corrupt-cache\n" +
		"fatal: something unexpected went wrong\n"
	if got, _ := classifyGitFailure(originSide, exit, []byte(fetch), false); got != ErrUnclassified {
		t.Errorf("an unknown failure beside tls-named refs = %q, want unclassified", got)
	}

	// The reason in a rejected line still counts: it is git's explanation, not a ref name.
	quota := " ! [remote rejected] main -> main (repository size limit exceeded)\n"
	if got, _ := classifyGitFailure(replicaSide, exit, []byte(quota), false); got != ErrReplicaRejected {
		t.Errorf("a size-limit reason = %q, want replica_rejected", got)
	}
}

// A ref name must not steer the OOM classification either. Ref names cannot contain spaces, so this
// is a cheap guard on the scrubbing; the realistic route for server text is a rejected-ref reason,
// which TestClassifyGitFailure covers.
func TestClassifyGitFailureIgnoresSignalNamedRefs(t *testing.T) {
	exit := errors.New("exit status 128")
	fetch := "From https://git.example.com/group/repo-a\n" +
		" * [new branch]      index-pack-died-of-signal-9 -> index-pack-died-of-signal-9\n" +
		" ! [rejected]        died-of-signal-9 -> died-of-signal-9 (non-fast-forward)\n" +
		"fatal: something unexpected went wrong\n"
	if got, _ := classifyGitFailure(originSide, exit, []byte(fetch), false); got != ErrUnclassified {
		t.Errorf("an unknown failure beside signal-named refs = %q, want unclassified", got)
	}
}

// With no fatal:/error: line, the detail used to fall back to the last line, which could be a ref
// status line carrying a branch name.
func TestClassifyGitFailureDetailNeverCarriesRefNames(t *testing.T) {
	exit := errors.New("exit status 128")
	stderr := " * [new tag]         release-tag-1 -> release-tag-1\n * [new tag]         release-tag-2 -> release-tag-2\n"

	typ, detail := classifyGitFailure(replicaSide, exit, []byte(stderr), false)
	if typ != ErrUnclassified {
		t.Fatalf("error_type = %q, want unclassified", typ)
	}
	if strings.Contains(detail, "release-tag") || strings.Contains(detail, "->") {
		t.Errorf("detail %q carries a ref name", detail)
	}
	if detail == "" {
		t.Error("unclassified must still carry a detail")
	}
}

// A hostname from a proxy error is customer infrastructure; the detail must never carry a host.
func TestClassifyGitFailureDetailNeverCarriesAHost(t *testing.T) {
	exit := errors.New("exit status 128")
	stderr := "fatal: unable to access 'https://git.example.com/x/': Weird proxy trouble via proxy.corp.example:3128 and 10.1.2.3"

	_, detail := classifyGitFailure(originSide, exit, []byte(stderr), false)
	for _, leak := range []string{"proxy.corp.example", "3128", "10.1.2.3", "example"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail %q leaks %q", detail, leak)
		}
	}
}

func TestClassifyGitFailureProxyResolveFailureIsUnreachable(t *testing.T) {
	exit := errors.New("exit status 128")
	stderr := "fatal: unable to access 'https://git.example.com/x/': Could not resolve proxy: proxy.corp.example"
	if got, detail := classifyGitFailure(originSide, exit, []byte(stderr), false); got != ErrOriginUnreachable || detail != "" {
		t.Errorf("got (%q, %q), want origin_unreachable with no detail", got, detail)
	}
}

func TestIsCorruptMirror(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"not a repository", "fatal: not a git repository: '/m/abc.git'", true},
		{"corrupt object file", "error: object file /m/abc.git/objects/ab/cd is corrupt\nfatal: loose object is corrupt", true},
		{"bad object", "error: bad object refs/heads/main", true},
		{"the origin reporting its own corruption", "remote: error: object 1234 is corrupt\nfatal: the remote end hung up unexpectedly", false},
		{"a ref named corrupt", " * [new branch]      fix-corrupt-cache -> fix-corrupt-cache\nfatal: unable to access 'https://git.example.com/': Connection refused", false},
		{"unrelated failure", "fatal: unable to access 'https://git.example.com/': Could not resolve host: git.example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCorruptMirror([]byte(tt.stderr)); got != tt.want {
				t.Errorf("isCorruptMirror(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestClassifyGitFailureUnclassifiedDetailIsSanitized(t *testing.T) {
	exit := errors.New("exit status 128")
	stderr := "fatal: weird failure contacting https://secret-host.example.com/group/repo.git near '/var/lib/gitsync/mirrors/abc.git' while syncing\n"

	typ, detail := classifyGitFailure(originSide, exit, []byte(stderr), false)
	if typ != ErrUnclassified {
		t.Fatalf("error_type = %q, want unclassified", typ)
	}
	if detail == "" {
		t.Fatal("unclassified must carry a non-empty detail")
	}
	for _, leak := range []string{"secret-host", "https://", "/var/lib", "group/repo", "example.com"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail %q leaks %q", detail, leak)
		}
	}
	if strings.ContainsAny(detail, "\n\r\x00") {
		t.Errorf("detail must be a single clean line, got %q", detail)
	}
}

func TestClassifyGitFailureUnclassifiedDetailIsBounded(t *testing.T) {
	exit := errors.New("exit status 128")
	long := "fatal: " + strings.Repeat("x", 5000)
	_, detail := classifyGitFailure(replicaSide, exit, []byte(long), false)
	if len(detail) == 0 || len(detail) > maxErrorDetail {
		t.Errorf("detail length = %d, want 1..%d", len(detail), maxErrorDetail)
	}
}
