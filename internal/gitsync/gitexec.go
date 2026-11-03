package gitsync

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// This file is the connector's single place for building a git subprocess environment. Every git
// invocation (the reachability probe and the mirror sync) goes through it, so the lockdown applied
// to one cannot drift from the other.

// gitBaseEnv builds a minimal, explicit environment for git. It never copies os.Environ()
// wholesale: an inherited GIT_SSL_NO_VERIFY, url.*.insteadOf, http.proxy, GIT_TRACE, or a credential
// helper could weaken TLS, redirect the target, or dump the Authorization header. Only PATH, an
// empty HOME, locale, and explicitly chosen proxy/CA variables are passed through. It carries no
// credential; see credConfig.
func gitBaseEnv(home string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"LANGUAGE=",
		"GIT_CONFIG_NOSYSTEM=1",
		// GIT_CONFIG_GLOBAL needs git >= 2.32; on 2.31 it is ignored, but HOME is an empty temp dir
		// so there is no ~/.gitconfig to read either way. (GitVersionOK gates the image at >= 2.31.)
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
	}
	// Pass through only proxy and CA configuration, deliberately.
	for _, k := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// gitConfigEnv appends cfg to base as GIT_CONFIG_COUNT/KEY_n/VALUE_n (git >= 2.31), which keeps
// injected configuration — including a credential header — out of argv and the URL. Note:
// GIT_CONFIG_VALUE_* is readable via /proc/<pid>/environ (mode 0400) by the same user — the same
// exposure as the connector's own environment, not secret from the owner, but not visible to other
// users or in a process list the way an argv would be.
func gitConfigEnv(base []string, cfg [][2]string) []string {
	env := append([]string(nil), base...)
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]))
		env = append(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// credConfig returns the git config pairs for contacting remoteURL: the fixed hardening pairs, plus
// — when auth is set — an Authorization header scoped to remoteURL. The header is keyed
// http.<url>/.extraHeader, so git sends it only to requests whose URL falls under remoteURL (same
// scheme, host and port, path-prefix on a segment boundary) and never to another host. This is what
// stops the origin credential from riding to the replica, or the push token to the origin, even if a
// redirect or a mismatched URL were ever followed.
func credConfig(remoteURL, auth, username string, token Secret) [][2]string {
	cfg := [][2]string{
		// Never let the Authorization header follow a redirect to another host or to http.
		{"http.followRedirects", "false"},
		{"credential.helper", ""},
		{"core.askPass", ""},
	}
	var header string
	switch auth {
	case "bearer":
		header = "Authorization: Bearer " + token.Reveal()
	case "basic":
		creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + token.Reveal()))
		header = "Authorization: Basic " + creds
	default:
		return cfg
	}
	return append(cfg, [2]string{"http." + strings.TrimSuffix(remoteURL, "/") + "/.extraHeader", header})
}

// newTempHome creates an empty directory to use as git's HOME, so git cannot read the invoking
// user's ~/.gitconfig or credential store, and returns it with its cleanup function.
func newTempHome() (string, func(), error) {
	home, err := os.MkdirTemp("", "gitsync-home-")
	if err != nil {
		return "", nil, fmt.Errorf("git home setup failed: %w", err)
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}
