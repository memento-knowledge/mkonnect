package gitsync

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// maxConfigFileSize bounds the provider config read at load time. A real file is a few kilobytes;
// anything larger is a misdirected path and is rejected rather than read into memory.
const maxConfigFileSize = 1 << 20

// fchmod tightens an already-open file, overridable in tests to exercise the read-only-mount branch
// of readSecureFile (as the file's owner, chmod cannot be made to fail portably). Mirrors the seam
// in internal/creds.
var fchmod = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

// readSecureFile reads a credential-bearing file whose location decides where secrets are sent. The
// file is opened once and every check (type, mode, owner, size) and the read itself go through that
// one descriptor, so the file that is vetted is exactly the file that is read: nothing can be
// swapped in between a path-based check and a path-based read. A symlink at path is followed,
// because Kubernetes projects Secret and ConfigMap volumes through one.
//
// The mode rule: the file is tightened to 0600 where possible. If that fails — most often a
// read-only mount such as a Kubernetes Secret — the existing mode is accepted only when it denies
// world access and group write (group read is allowed for fsGroup Secret mounts). This mirrors the
// API connector's credential store so the two behave alike. what names the file in errors ("git-sync
// config"); no error includes the file's contents. An open error is returned unchanged so callers
// can test it with os.IsNotExist.
//
// Advisory only (a warning, never a rejection): the file is owned by someone other than the running
// user or root, or its directory lets other users rename entries within it.
func readSecureFile(path, what string, maxSize int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %s is not a regular file", what, path)
	}
	if info.Size() > maxSize {
		return nil, fmt.Errorf("%s %s is larger than the %d-byte limit", what, path, maxSize)
	}
	if err := fchmod(f, 0o600); err != nil && !readOnlyModeAcceptable(info.Mode()) {
		return nil, fmt.Errorf("%s %s has unsafe permissions and chmod failed; restrict it to the owner, allowing at most group read (e.g. 0600 or 0640)", what, path)
	}
	warnUnusualOwnership(path, what, info)

	// Read one byte past the limit so a file that grew after the Stat is rejected, not truncated.
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxSize {
		return nil, fmt.Errorf("%s %s is larger than the %d-byte limit", what, path, maxSize)
	}
	return b, nil
}

// readOnlyModeAcceptable reports whether a file whose mode could not be corrected to 0600 is
// still safe: no group-write (0o020) and no "other" bits (0o007).
func readOnlyModeAcceptable(mode os.FileMode) bool {
	return mode.Perm()&0o027 == 0
}

// foreignOwner reports whether a file owned by uid is owned by neither the running user nor root.
func foreignOwner(uid, euid int) bool {
	return uid != euid && uid != 0
}

// dirLetsOthersRename reports whether a directory with this mode lets any user rename or replace
// the files in it: world-writable without the sticky bit.
func dirLetsOthersRename(mode os.FileMode) bool {
	return mode.Perm()&0o002 != 0 && mode&os.ModeSticky == 0
}

// warnUnusualOwnership logs, without rejecting, the two conditions under which another local user
// could replace or alter the file. They are warnings because existing deployments may legitimately
// have them (for example a volume provisioned by another uid), and rejecting would turn an upgrade
// into an outage.
func warnUnusualOwnership(path, what string, info os.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && foreignOwner(int(st.Uid), os.Geteuid()) {
		slog.Warn("credential file is owned by another user; restrict it to the user running the connector", "file", what, "path", path)
	}
	if d, err := os.Stat(filepath.Dir(path)); err == nil && dirLetsOthersRename(d.Mode()) {
		slog.Warn("credential file's directory is writable by other users; restrict it", "file", what, "path", path)
	}
}
