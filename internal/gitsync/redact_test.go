package gitsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A server can put anything in the error it sends, including the Authorization header it was given.
// The text after "remote error:" or "remote:" is the remote's, not git's, and is never reported.
func TestRawDetailWithholdsTextTheRemoteSent(t *testing.T) {
	exit := errors.New("exit status 128")
	for name, tc := range map[string]struct{ stderr, want string }{
		"err packet":        {"fatal: remote error: Authorization: Bearer abc123SECRET\n", "fatal: remote error: <text from the remote withheld>"},
		"err packet, case":  {"fatal: Remote Error: token SECRET\n", "fatal: Remote Error: <text from the remote withheld>"},
		"sideband":          {"remote: SECRET is not allowed\n", "remote: <text from the remote withheld>"},
		"unpack":            {"error: remote unpack failed: SECRET\n", "error: remote unpack failed: <text from the remote withheld>"},
		"git's own words":   {"fatal: the remote end hung up unexpectedly\n", "fatal: the remote end hung up unexpectedly"},
		"label only":        {"fatal: remote error:\n", "fatal: remote error:"},
		"after other lines": {"Cloning...\nfatal: remote error: SECRET\n", "fatal: remote error: <text from the remote withheld>"},
	} {
		t.Run(name, func(t *testing.T) {
			got := rawDetail(exit, []byte(tc.stderr))
			if got != tc.want || strings.Contains(got, "SECRET") {
				t.Errorf("rawDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactSecrets(t *testing.T) {
	origin := LocalEntry{Auth: "basic", Username: "svc", Token: Secret("origin-token-0123456789")}
	push := Secret("push-token-abcdefghij")
	forms := secretForms(origin, push)

	basic := "c3ZjOm9yaWdpbi10b2tlbi0wMTIzNDU2Nzg5" // base64("svc:origin-token-0123456789")
	in := "a origin-token-0123456789 b push-token-abcdefghij c Basic " + basic + " d"
	got := string(redactSecrets([]byte(in), forms))
	if got != "a <redacted> b <redacted> c Basic <redacted> d" {
		t.Errorf("redacted = %q", got)
	}
	if got := string(redactSecrets([]byte("nothing here"), forms)); got != "nothing here" {
		t.Errorf("text without a secret changed: %q", got)
	}

	// A short push token would match ordinary words, so it is not redacted.
	if forms := secretForms(LocalEntry{}, Secret("short")); len(forms) != 0 {
		t.Errorf("forms for a short secret = %q, want none", forms)
	}
}

// Whatever git prints on stderr, the credentials of the sync that ran it are gone before anything
// reads it.
func TestSyncAttemptRedactsStderrBeforeItIsClassified(t *testing.T) {
	const tok = "origin-token-0123456789"
	var seen []byte
	d := syncDeps{
		mirrors: Mirrors{Root: t.TempDir()},
		now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
		run: func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
			seen = []byte("error: something about " + tok + " happened\n")
			return seen, errors.New("exit status 1")
		},
	}
	res := syncRepo(context.Background(), d, syncTask{
		RepoID: "r", CanonicalURL: "https://git.example.test/r.git", ReplicaURL: "https://replica.example.test/r.git",
		Origin:    LocalEntry{Auth: "bearer", Token: Secret(tok)},
		PushToken: Secret("push-token-abcdefghij"),
	})
	if res.State != StateError || strings.Contains(res.ErrorDetail, tok) {
		t.Errorf("result = %+v: the detail must not carry the token", res)
	}
	if !strings.Contains(res.ErrorDetail, redacted) {
		t.Errorf("detail %q does not show the redaction", res.ErrorDetail)
	}
}
