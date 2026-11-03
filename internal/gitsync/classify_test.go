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
		{"replica size limit", replicaSide, exit, "remote: error: repository size limit exceeded\nTo https://replica.example.com/r.git\n ! [remote rejected] main -> main (pre-receive hook declined)", false, ErrRepositoryOverLimit},
		{"replica pack too large", replicaSide, exit, "error: remote unpack failed: pack exceeds maximum allowed size", false, ErrRepositoryOverLimit},
		{"replica 413", replicaSide, exit, "fatal: unable to access 'https://replica.example.com/r/': The requested URL returned error: 413", false, ErrRepositoryOverLimit},
		{"replica hook declined", replicaSide, exit, "To https://replica.example.com/r.git\n ! [remote rejected] main -> main (pre-receive hook declined)", false, ErrReplicaRejected},
		{"replica deadline is push timeout", replicaSide, timedOut, "", false, ErrPushTimeout},

		// Local faults, either side.
		{"disk full on origin fetch", originSide, exit, "fatal: write error: No space left on device", false, ErrLocalDiskExhausted},
		{"disk full on push", replicaSide, exit, "error: unable to write file: No space left on device", false, ErrLocalDiskExhausted},
		// A "remote:" line is the replica speaking: its full disk is not ours.
		{"replica-side disk full is not local", replicaSide, exit, "remote: fatal: write error: No space left on device\n ! [remote rejected] main -> main (unpacker error)", false, ErrReplicaRejected},
		{"out of memory", originSide, exit, "fatal: Out of memory, malloc failed (tried to allocate 1073741824 bytes)", false, ErrJobOutOfMemory},
		{"killed with no deadline is oom", replicaSide, errors.New("signal: killed"), "", false, ErrJobOutOfMemory},

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
