package gitsync

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// maxConfigFileSize bounds the provider config read at load time. A real file is a few kilobytes;
// anything larger is a misdirected path and is rejected rather than read into memory.
const maxConfigFileSize = 1 << 20

// maxTokenFileSize bounds a token file. A git credential is a short string, so anything larger is
// a misdirected path. It matches the registration token file's limit.
const maxTokenFileSize = 4096

// rejectForeignConfigOwner decides what happens to a provider config file that is owned by neither
// the running user nor root and holds only inline tokens. For one release it only warns (an
// upgrade must not turn into an outage for a volume that another uid provisioned); flip this to
// true to reject it. A config that uses token_file is always rejected when foreign-owned, because
// that feature is new and the config then decides which local file is sent to which host.
const rejectForeignConfigOwner = false

// fchmod tightens an already-open file, overridable in tests to exercise the read-only-mount branch
// of the config policy (as the file's owner, chmod cannot be made to fail portably). Mirrors the
// seam in internal/creds.
var fchmod = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

// effectiveUID and fileOwnerUID are seams so tests can present a file as owned by another user
// (a test cannot chown to an arbitrary uid without root).
var (
	effectiveUID = os.Geteuid
	fileOwnerUID = func(info os.FileInfo) (uid int, ok bool) {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return int(st.Uid), true
	}
)

// tokenFileReader is how New reads a token file; a test replaces it to count reads.
var tokenFileReader = readTokenFile

// readVetted reads a credential-bearing file whose location or content decides where secrets are
// sent. The file is opened once and every check (type, size, and whatever vet adds) and the read
// itself go through that one descriptor, so the file that is vetted is exactly the file that is
// read: nothing can be swapped in between a path-based check and a path-based read. A symlink at
// path is followed, because Kubernetes projects Secret and ConfigMap volumes through one; the
// descriptor's mode and owner are those of the final target. what names the file in errors; no
// error includes the file's contents. An open error is returned unchanged so callers can test it
// with errors.Is(err, fs.ErrNotExist).
func readVetted(path, what string, maxSize int64, vet func(f *os.File, info os.FileInfo) error) ([]byte, error) {
	// O_NONBLOCK so that opening a FIFO (or a symlink to one) returns at once instead of waiting for
	// a writer; the regular-file check below then rejects it. It has no effect on a regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
	if err := vet(f, info); err != nil {
		return nil, err
	}

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

// providerConfigFile is the vetted content of the provider config.
type providerConfigFile struct {
	data []byte
	// foreignOwner reports that the file is owned by neither the running user nor root (or that its
	// owner could not be determined). The caller decides whether that is fatal, because it depends on
	// what the file asks for (see rejectForeignConfigOwner).
	foreignOwner bool
	// loose reports that the file's mode is wider than the connector accepts and was not (or could
	// not be) tightened. Only a look (readProviderConfigMode with tighten false) returns a file in
	// that state; it is part of what the look saw, so a change of mode alone is noticed.
	loose bool
}

// readProviderConfig reads the provider config under the config policy.
//
// The mode rule: the file is tightened to 0600 where possible. If that fails — most often a
// read-only mount such as a Kubernetes Secret — the existing mode is accepted only when it denies
// world access and group write (group read is allowed for fsGroup Secret mounts). This mirrors the
// API connector's credential store so the two behave alike.
//
// The owner rule: with rejectForeign the owner must be the running user or root, checked before the
// mode is touched; otherwise the owner is only reported for the caller to judge, and a file owned
// by someone else is never chmod-ed (it is accepted only if its mode is already safe).
//
// Advisory only (a warning, never a rejection): the directory lets other users rename entries
// within it.
func readProviderConfig(path string, maxSize int64, rejectForeign bool) (providerConfigFile, error) {
	return readProviderConfigMode(path, maxSize, rejectForeign, true)
}

// readProviderConfigMode is readProviderConfig with the choice of whether it may touch the file. With
// tighten false (a look, as the live reload polls with) it changes nothing and logs nothing: a mode
// that the real read would tighten is reported in loose instead of being judged, because whether it
// can be tightened is for that read to find out. Every other rule is the same, so a look and a read
// of the same file agree about everything but the mode.
func readProviderConfigMode(path string, maxSize int64, rejectForeign, tighten bool) (providerConfigFile, error) {
	const what = "git-sync config"
	var out providerConfigFile
	data, err := readVetted(path, what, maxSize, func(f *os.File, info os.FileInfo) error {
		out.foreignOwner = foreignOwner(info)
		if out.foreignOwner && rejectForeign {
			return fmt.Errorf("%s %s is owned by another user; it must be owned by the user running the connector or by root", what, path)
		}
		if out.foreignOwner {
			// Never tighten a file that belongs to someone else (as root, chmod would succeed and change
			// another user's file, then the caller might still refuse it); accept it only if its mode is
			// already safe.
			if !readOnlyModeAcceptable(info.Mode()) {
				return fmt.Errorf("%s %s is owned by another user and has unsafe permissions; it must be owned by the user running the connector or by root, with at most group read (e.g. 0600 or 0640)", what, path)
			}
		} else if !tighten {
			out.loose = !readOnlyModeAcceptable(info.Mode())
		} else if err := fchmod(f, 0o600); err != nil && !readOnlyModeAcceptable(info.Mode()) {
			return fmt.Errorf("%s %s has unsafe permissions and chmod failed; restrict it to the owner, allowing at most group read (e.g. 0600 or 0640)", what, path)
		}
		if tighten {
			warnWorldWritableDir(path, what)
		}
		return nil
	})
	out.data = data
	return out, err
}

// readTokenFile reads a token file under the token policy. what names the file in errors and must
// be safe to log (it carries the provider name). Unlike the config, the file is never chmod-ed: it
// belongs to whoever provisioned the secret and is often a read-only mount.
//
//   - Writable by group or others: rejected, since anyone with write access could swap the
//     credential the connector sends.
//   - Owned by neither the running user nor root (or owner unknown): rejected, for the same reason.
//   - Readable by group or others: only a warning, logged once per file. Docker secrets default to
//     0444 and a read-only Kubernetes Secret mount cannot be chmod-ed, so rejecting would make the
//     feature unusable there.
func readTokenFile(path, what string) ([]byte, error) {
	data, err := readVetted(path, what, maxTokenFileSize, func(_ *os.File, info os.FileInfo) error {
		mode := info.Mode().Perm()
		if mode&0o022 != 0 {
			return fmt.Errorf("%s %s is writable by group or others; restrict it to the owner, allowing at most read access for the group (e.g. 0600, 0640 or 0440)", what, path)
		}
		if foreignOwner(info) {
			return fmt.Errorf("%s %s is owned by another user; it must be owned by the user running the connector or by root", what, path)
		}
		warnWhile(mode&0o044 != 0, "read", path,
			"credential file is readable by group or other users; restrict it to the user running the connector where the platform allows (a read-only secret mount may not)", "file", what, "path", path)
		warnWorldWritableDir(path, what)
		return nil
	})
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "open" {
		// The open error repeats the path; report it once, in the same words as the other errors.
		// The cause is deliberately not wrapped: New treats errors.Is(err, fs.ErrNotExist) from the
		// config file as "no config yet, empty store", and a missing token file must never be mistaken
		// for that by a caller that copies the idiom.
		err := fmt.Errorf("%s %s could not be opened: %v", what, path, pe.Err)
		if errors.Is(pe.Err, fs.ErrNotExist) {
			return nil, &tokenFileMissingError{err: err}
		}
		return nil, err
	}
	return data, err
}

// tokenFileMissingError is the error for a token file that does not exist. It is deliberately not
// errors.Is(fs.ErrNotExist) (see readTokenFile); the loader recognises it by type, so that a reload
// can tell a credential that was removed from one that could not be read.
type tokenFileMissingError struct{ err error }

func (e *tokenFileMissingError) Error() string { return e.err.Error() }

// readOnlyModeAcceptable reports whether a file whose mode could not be corrected to 0600 is
// still safe: no group-write (0o020) and no "other" bits (0o007).
func readOnlyModeAcceptable(mode os.FileMode) bool {
	return mode.Perm()&0o027 == 0
}

// foreignOwner reports whether the file is owned by neither the running user nor root. An owner
// that cannot be determined counts as foreign, so the check fails closed.
func foreignOwner(info os.FileInfo) bool {
	uid, ok := fileOwnerUID(info)
	return !ok || isForeignUID(uid, effectiveUID())
}

// isForeignUID reports whether a file owned by uid is owned by neither the running user nor root.
func isForeignUID(uid, euid int) bool {
	return uid != euid && uid != 0
}

// dirLetsOthersRename reports whether a directory with this mode lets any user rename or replace
// the files in it: world-writable without the sticky bit.
func dirLetsOthersRename(mode os.FileMode) bool {
	return mode.Perm()&0o002 != 0 && mode&os.ModeSticky == 0
}

// warnWorldWritableDir logs, without rejecting, that another local user could replace the file by
// renaming something over it (or by swapping it for a symlink to another file the connector can
// read). It is a warning because existing deployments may legitimately have it and rejecting would
// turn an upgrade into an outage. Logged once per file while the condition holds.
func warnWorldWritableDir(path, what string) {
	d, err := os.Stat(filepath.Dir(path))
	warnWhile(err == nil && dirLetsOthersRename(d.Mode()), "dir", path,
		"credential file's directory is writable by other users; restrict it", "file", what, "path", path)
}

// warned records which (condition, file) pairs already produced their warning, so a process that
// loads the config repeatedly (a reload loop) logs each once. An entry is cleared when the file is
// seen without the condition, so the condition returning later warns again.
var warned sync.Map

// warnWhile logs msg once per kind and path while cond holds, and forgets the pair once it does not.
func warnWhile(cond bool, kind, path, msg string, attrs ...any) {
	key := kind + "\x00" + path
	if !cond {
		warned.Delete(key)
		return
	}
	if _, seen := warned.LoadOrStore(key, struct{}{}); !seen {
		slog.Warn(msg, attrs...)
	}
}
