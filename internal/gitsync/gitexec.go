package gitsync

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// This file is the connector's single place for building a git subprocess environment. Every git
// invocation (the reachability probe and the mirror sync) goes through it, so the lockdown applied
// to one cannot drift from the other.

// gitBaseEnv builds a minimal, explicit environment for git. It never copies os.Environ()
// wholesale: an inherited GIT_SSL_NO_VERIFY, url.*.insteadOf, http.proxy, GIT_TRACE, or a credential
// helper could weaken TLS, redirect the target, or dump the Authorization header. Only PATH, an
// empty HOME, locale, a temp directory, and explicitly chosen proxy/CA variables are passed
// through. It carries no credential; see credConfig.
//
// TMPDIR is home itself, not the inherited value, so git's own temporary files land in a private
// (0700) writable directory that cleanup removes and the startup sweep covers, rather than
// falling back to /tmp, which may be read-only.
func gitBaseEnv(home string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + home,
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

// packConfig returns the git config pairs for the pack limits: pack.threads (the thread count of
// index-pack on fetch and pack-objects on push) and pack.windowMemory (the per-thread delta-window
// memory of pack-objects, which a fetch does not run, in MiB). A zero or negative limit means git's
// own default and adds nothing. The values are formatted from integers only, so nothing that came
// from the wire as text can reach git through here.
func packConfig(l Limits) [][2]string {
	var cfg [][2]string
	if l.PackThreads > 0 {
		cfg = append(cfg, [2]string{"pack.threads", strconv.Itoa(l.PackThreads)})
	}
	if l.PackWindowMemoryMB > 0 {
		cfg = append(cfg, [2]string{"pack.windowMemory", strconv.Itoa(l.PackWindowMemoryMB) + "m"})
	}
	return cfg
}

// maintenanceConfig makes a fetch's automatic repack/gc run in the foreground when a repository
// ceiling is in force. Detached, it would still be rewriting packs (briefly doubling their apparent
// size) while the ceiling check measures the mirror, and could report a repository over the limit
// that is not.
func maintenanceConfig(l Limits) [][2]string {
	if l.MaxRepositoryMB <= 0 {
		return nil
	}
	return [][2]string{{"gc.autoDetach", "false"}, {"maintenance.autoDetach", "false"}}
}

// tempHomePrefix names every directory newTempHome creates; the startup sweep removes only
// entries carrying it.
const tempHomePrefix = "gitsync-home-"

// TempRoot returns the directory under which per-operation temp homes are created, creating it
// (mode 0700) if missing, and verifies it is writable by creating and removing a probe directory,
// so an unusable location (for example an existing read-only mount) fails at startup rather than
// on the first sync. It is GIT_SYNC_TMP_DIR when set, otherwise os.TempDir(), which honours
// TMPDIR. Whichever source is used, the result must be an absolute path: a relative one is an
// error, never a silent fallback. A deployment with a read-only root filesystem points it at a
// writable volume.
func TempRoot() (string, error) {
	root, err := resolveTempRoot()
	if err != nil {
		return "", err
	}
	probe, err := os.MkdirTemp(root, tempHomePrefix)
	if err != nil {
		return "", fmt.Errorf("temp directory %q is not writable (set GIT_SYNC_TMP_DIR or TMPDIR to a writable absolute path): %w", root, err)
	}
	if err := os.Remove(probe); err != nil {
		return "", fmt.Errorf("temp directory %q is not usable: %w", root, err)
	}
	return root, nil
}

// resolveTempRoot picks the temp root from the environment, checks it is absolute, and creates it.
func resolveTempRoot() (string, error) {
	root, source := os.TempDir(), "TMPDIR"
	if v := os.Getenv("GIT_SYNC_TMP_DIR"); v != "" {
		root, source = v, "GIT_SYNC_TMP_DIR"
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", source, root)
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("temp directory %q (from %s) is not usable: %w", root, source, err)
	}
	return root, nil
}

// newTempHome creates an empty directory under the temp root to use as git's HOME, so git cannot
// read the invoking user's ~/.gitconfig or credential store, and returns it with its cleanup
// function. git is also told (via gitBaseEnv) to use that directory for its own temporary files.
func newTempHome() (string, func(), error) {
	root, err := resolveTempRoot()
	if err != nil {
		return "", nil, fmt.Errorf("git home setup failed: %w", err)
	}
	home, err := os.MkdirTemp(root, tempHomePrefix)
	if err != nil {
		return "", nil, fmt.Errorf("git home setup failed: %w", err)
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}

// SweepStaleTempHomes removes leftover temp homes (a crash or kill skips newTempHome's cleanup)
// from root, so they do not accumulate when the temp directory is on a persistent volume. Call it
// once at startup. It is deliberately conservative:
//   - only direct children of root whose name starts with the temp-home prefix are considered;
//   - only real directories are removed (checked with Lstat, so a symlink with a matching name is
//     left alone and never followed; os.RemoveAll does not follow symlinks inside a tree either);
//   - only entries last modified more than olderThan ago, which spares directories created
//     moments ago.
//
// The temp root must not be shared between running instances: a directory's mtime reflects the
// last entry added or removed in it, not liveness, so this cannot tell an orphaned home from one
// in use by another running process. One instance per volume or container is the supported setup.
// The daemon calls this before it starts any operation of its own.
//
// It is best effort: it keeps going after a failure and returns how many directories it removed
// together with the first error encountered. A missing root is not an error.
func SweepStaleTempHomes(root string, olderThan time.Duration) (removed int, err error) {
	if !filepath.IsAbs(root) {
		return 0, fmt.Errorf("temp root must be an absolute path, got %q", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var first error
	note := func(e error) {
		if first == nil {
			first = e
		}
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tempHomePrefix) {
			continue
		}
		path := filepath.Join(root, e.Name())
		fi, lerr := os.Lstat(path)
		if lerr != nil {
			if !errors.Is(lerr, fs.ErrNotExist) {
				note(lerr)
			}
			continue
		}
		if !fi.Mode().IsDir() || fi.ModTime().After(cutoff) {
			continue
		}
		if rerr := os.RemoveAll(path); rerr != nil {
			note(rerr)
			continue
		}
		removed++
	}
	return removed, first
}
