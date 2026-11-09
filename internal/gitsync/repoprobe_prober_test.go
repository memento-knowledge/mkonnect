package gitsync

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests drive the repository probe (gitProber.ProbeRepository) against the real git binary over
// HTTP, so the classification is checked against what git really prints, and with a fake runner for the
// cases a real server cannot produce.

func repoProbeFixture(t *testing.T) (*httpFixture, RepoTarget) {
	t.Helper()
	f := newHTTPFixture(t)
	entry := LocalEntry{BaseURL: f.origin.URL + "/", Auth: "bearer", Token: Secret(httpOriginToken)}
	return f, RepoTarget{entry: entry, url: f.origin.URL + "/origin.git"}
}

func probeRepo(t *testing.T, tg RepoTarget) (RepoCheck, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return NewGitProber().ProbeRepository(ctx, tg)
}

func TestProbeRepositoryOverRealGit(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *httpFixture, tg *RepoTarget)
		want   RepoCheck
	}{
		{"credential and path work", func(*httpFixture, *RepoTarget) {}, RepoCheck{Result: OriginReachable}},
		{"credential refused (401)", func(_ *httpFixture, tg *RepoTarget) { tg.entry.Token = "wrong-origin-token-xyz" },
			RepoCheck{Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied, CredentialRejected: true}},
		{"no credential configured, server asks for one (401)", func(_ *httpFixture, tg *RepoTarget) { tg.entry.Auth, tg.entry.Token = "", "" },
			RepoCheck{Result: OriginAccessDenied, ErrorType: ErrOriginAccessDenied}},
		{"credential lacks access (403)", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(403) },
			RepoCheck{Result: OriginReachable, ErrorType: ErrOriginAccessDenied}},
		{"repository does not exist (404)", func(f *httpFixture, tg *RepoTarget) { tg.url = f.origin.URL + "/missing.git" },
			RepoCheck{Result: OriginReachable, ErrorType: ErrOriginAccessDenied}},
		{"redirect 301", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(301) },
			RepoCheck{Result: OriginReachable, ErrorType: ErrOriginURLRedirected}},
		{"redirect 302", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(302) },
			RepoCheck{Result: OriginReachable, ErrorType: ErrOriginURLRedirected}},
		{"redirect 307", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(307) },
			RepoCheck{Result: OriginReachable, ErrorType: ErrOriginURLRedirected}},
		{"redirect 308", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(308) },
			RepoCheck{Result: OriginReachable, ErrorType: ErrOriginURLRedirected}},
		{"server error (503)", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(503) },
			RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}},
		{"proxy wants authentication (407)", func(f *httpFixture, _ *RepoTarget) { f.origin.forceStatus.Store(407) },
			RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}},
		{"server is down", func(f *httpFixture, _ *RepoTarget) { f.origin.Close() },
			RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, tg := repoProbeFixture(t)
			tt.mutate(f, &tg)
			got, err := probeRepo(t, tg)
			if err != nil || got != tt.want {
				t.Fatalf("ProbeRepository = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// The sign-in case: the server's base URL answers a redirect (to a sign-in page) whatever the
// credential is, while the repository answers 401 for a refused one. The base-URL probe reads
// reachable; the repository probe reads access_denied.
func TestProbeRepositoryCatchesWhatTheBaseURLProbeMisses(t *testing.T) {
	f, tg := repoProbeFixture(t)
	f.origin.route(func(path string) int {
		if path == "/info/refs" { // the base URL, as git asks for it
			return http.StatusFound
		}
		return 0
	})
	tg.entry.Token = "wrong-origin-token-xyz"
	p := NewGitProber()

	base, err := p.Probe(context.Background(), tg.entry)
	if err != nil || base != OriginReachable {
		t.Fatalf("base URL probe with a wrong credential = %q, %v; want reachable (the gap the repository probe closes)", base, err)
	}
	repo, err := probeRepo(t, tg)
	if err != nil || repo.Result != OriginAccessDenied || !repo.CredentialRejected {
		t.Fatalf("repository probe with a wrong credential = %+v, %v; want access_denied", repo, err)
	}
	tg.entry.Token = Secret(httpOriginToken)
	if repo, err = probeRepo(t, tg); err != nil || repo != (RepoCheck{Result: OriginReachable}) {
		t.Fatalf("repository probe with the right credential = %+v, %v; want a clean pass", repo, err)
	}
}

// The credential goes only in a header for the exact repository URL, and git speaks protocol v2 (the
// default of every supported git; nothing in the probe's environment can change it).
func TestProbeRepositorySendsOnlyItsOwnCredentialOverProtocolV2(t *testing.T) {
	f, tg := repoProbeFixture(t)
	if _, err := probeRepo(t, tg); err != nil {
		t.Fatal(err)
	}
	hs := f.origin.authHeaders()
	if len(hs) == 0 {
		t.Fatal("the server was never contacted")
	}
	for _, h := range hs {
		if h != "Bearer "+httpOriginToken {
			t.Errorf("the server received %q", h)
		}
	}
	for _, p := range f.origin.gitProtocols() {
		if p != "version=2" {
			t.Errorf("Git-Protocol = %q, want version=2", p)
		}
	}
}

// The probe asks the server for branches only: under protocol v2 git then sends ref-prefix
// refs/heads/, so the refs a forge keeps for merge requests are not advertised. (Asking for HEAD as a
// pattern would not narrow the request: git sends no ref-prefix for a pattern.)
func TestProbeRepositoryAsksForBranchesOnly(t *testing.T) {
	f, tg := repoProbeFixture(t)
	if res, err := probeRepo(t, tg); err != nil || res.Result != OriginReachable {
		t.Fatalf("probe = %+v, %v", res, err)
	}
	var sawBranches bool
	for _, b := range f.origin.requestBodies() {
		if strings.Contains(b, "ref-prefix refs/heads/") {
			sawBranches = true
		}
		if strings.Contains(b, "ref-prefix HEAD") || strings.Contains(b, "ref-prefix refs/tags/") {
			t.Errorf("the request asks for more than branches:\n%q", b)
		}
	}
	if !sawBranches {
		t.Fatalf("no ls-refs request carried ref-prefix refs/heads/: %q", f.origin.requestBodies())
	}
}

// The probe's environment is the sync's: the same function builds both, so what a sync sends the probe
// sends, and nothing else.
func TestProbeRepositoryUsesTheSyncsOriginEnvironment(t *testing.T) {
	entry := LocalEntry{BaseURL: "https://git.example.com/", Auth: "basic", Username: "svc", Token: Secret("0123456789abcdef-A")}
	tg := RepoTarget{entry: entry, url: "https://git.example.com/group/repository.git"}
	var argv, env []string
	p := &gitProber{run: func(_ context.Context, a, e []string, _ string) ([]byte, error) {
		argv, env = a, e
		return nil, nil
	}}
	if _, err := p.ProbeRepository(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if want := []string{"git", "ls-remote", "--heads", "--", tg.url}; !slices.Equal(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	var home string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	syncEnv := syncDeps{}.originEnv(home, syncTask{CanonicalURL: tg.url, Origin: entry})
	if !slices.Equal(env, syncEnv) {
		t.Fatalf("probe environment differs from the sync's:\n probe %v\n sync  %v", env, syncEnv)
	}
	header := "http." + tg.url + "/.extraHeader"
	if !slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && strings.HasSuffix(kv, "="+header)
	}) {
		t.Errorf("the credential is not scoped to exactly %s", header)
	}
	if !slices.Contains(env, "GIT_CONFIG_KEY_0=http.followRedirects") || !slices.Contains(env, "GIT_CONFIG_VALUE_0=false") {
		t.Errorf("redirects are not disabled: %v", env)
	}
}

// The check reads a failure exactly as the sync does: for every sample of git's output, the error_type
// the probe reports is the one classifyGitFailure gives the sync.
func TestRepositoryProbeAgreesWithTheSyncsClassifier(t *testing.T) {
	cred := LocalEntry{Auth: "bearer", Token: Secret("0123456789abcdef-A")}
	samples := []struct{ name, stderr string }{
		{"401", "fatal: could not read Username for 'https://git.example.com': terminal prompts disabled"},
		{"401 status", "fatal: unable to access 'https://git.example.com/g/r.git/': The requested URL returned error: 401"},
		{"403", "fatal: unable to access 'https://git.example.com/g/r.git/': The requested URL returned error: 403"},
		{"404", "fatal: repository 'https://git.example.com/g/r.git/' not found"},
		{"301", "fatal: unable to access 'https://git.example.com/g/r.git/': The requested URL returned error: 301"},
		{"503", "fatal: unable to access 'https://git.example.com/g/r.git/': The requested URL returned error: 503"},
		{"407", "fatal: unable to access 'https://git.example.com/g/r.git/': The requested URL returned error: 407"},
		{"dns", "fatal: unable to access 'https://git.example.com/g/r.git/': Could not resolve host: git.example.com"},
		{"tls", "fatal: unable to access 'https://git.example.com/g/r.git/': SSL certificate problem"},
		{"unknown", "fatal: something git has never said before"},
		{"a 404 text in a remote line", "remote: returned error: 404\nfatal: unable to access 'https://git.example.com/g/r.git/': The requested URL returned error: 503"},
	}
	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			stderr := []byte(s.stderr)
			syncType, _ := classifyGitFailure(originSide, errExit, stderr, false)
			got, err := repoCheckOf(cred, errExit, stderr)
			if err != nil {
				t.Fatal(err)
			}
			want := syncType
			if want == ErrUnclassified {
				want = ErrOriginUnreachable // the one place the probe maps, per the wire table
			}
			if got.ErrorType != want {
				t.Errorf("probe error_type %q, sync error_type %q", got.ErrorType, syncType)
			}
			// The probe's gate evidence is the sync's: a credential-rejected failure of a provider with a credential.
			if got.CredentialRejected != (syncType == ErrOriginAccessDenied && originCredentialRejected(stderr)) {
				t.Errorf("CredentialRejected = %v disagrees with the sync's rule", got.CredentialRejected)
			}
		})
	}
}

func TestProbeRepositoryLocalFaultsAndCancellation(t *testing.T) {
	tg := RepoTarget{entry: LocalEntry{BaseURL: "https://git.example.com/", Auth: "bearer", Token: Secret("0123456789abcdef-A")}, url: "https://git.example.com/group/repository.git"}

	t.Run("git cannot start", func(t *testing.T) {
		p := &gitProber{run: func(context.Context, []string, []string, string) ([]byte, error) {
			return nil, &exec.Error{Name: "git", Err: exec.ErrNotFound}
		}}
		if got, err := p.ProbeRepository(context.Background(), tg); err == nil {
			t.Fatalf("got %+v, want a local fault", got)
		}
	})
	for _, text := range []string{"fatal: No space left on device", "fatal: out of memory"} {
		t.Run(text, func(t *testing.T) {
			p := &gitProber{run: func(context.Context, []string, []string, string) ([]byte, error) {
				return []byte(text + " writing /srv/some/private/path"), errExit
			}}
			got, err := p.ProbeRepository(context.Background(), tg)
			if err == nil {
				t.Fatalf("got %+v, want a local fault, never an origin result", got)
			}
			if strings.Contains(err.Error(), "/srv") || strings.Contains(err.Error(), "path") {
				t.Errorf("the fault carries git's output: %v", err)
			}
		})
	}
	t.Run("a git killed at the deadline is unreachable, not a fault", func(t *testing.T) {
		p := &gitProber{timeout: 20 * time.Millisecond, run: func(ctx context.Context, _, _ []string, _ string) ([]byte, error) {
			<-ctx.Done()
			return nil, errors.New("signal: killed") // what a git that outlived SIGTERM reports
		}}
		got, err := p.ProbeRepository(context.Background(), tg)
		if err != nil || got != (RepoCheck{Result: OriginUnreachable, ErrorType: ErrOriginUnreachable}) {
			t.Fatalf("got %+v, %v; want unreachable/origin_unreachable", got, err)
		}
	})
	t.Run("a probe stopped from outside says nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		p := &gitProber{run: func(ctx context.Context, _, _ []string, _ string) ([]byte, error) {
			cancel()
			<-ctx.Done()
			// What git prints while it is being killed may even look like a 401.
			return []byte("fatal: Authentication failed"), errors.New("signal: terminated")
		}}
		got, err := p.ProbeRepository(ctx, tg)
		if err == nil || got != (RepoCheck{}) {
			t.Fatalf("got %+v, %v; want no result and an error", got, err)
		}
	})
}
