package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxRegistrationTokenFileSize bounds how much of REGISTRATION_TOKEN_FILE is read. A
// registration token is a short opaque string, so anything larger is a misconfigured
// path and is rejected rather than truncated.
const maxRegistrationTokenFileSize = 4096

// registrationToken resolves the registration token. When REGISTRATION_TOKEN_FILE is
// set the token is read from that file and REGISTRATION_TOKEN is ignored; any failure
// to read the file is an error rather than a silent fallback to the environment
// variable. When it is unset, REGISTRATION_TOKEN is returned verbatim.
func registrationToken() (string, error) {
	path := os.Getenv("REGISTRATION_TOKEN_FILE")
	if path == "" {
		return os.Getenv("REGISTRATION_TOKEN"), nil
	}
	token, err := readRegistrationTokenFile(path)
	if err != nil {
		return "", fmt.Errorf("REGISTRATION_TOKEN_FILE: %w", err)
	}
	return token, nil
}

// readRegistrationTokenFile reads and validates a registration token file. Errors
// describe the problem but never include file contents.
func readRegistrationTokenFile(path string) (string, error) {
	// Like CA_BUNDLE, the path is resolved by a long-running daemon whose working
	// directory a relative path cannot be relied on to match.
	if !filepath.IsAbs(path) {
		return "", errors.New("must be an absolute path")
	}

	// Stat before opening so a FIFO or device cannot block the open. Stat follows
	// symlinks, as a Kubernetes Secret mount requires (each key is a symlink into a
	// ..data directory), so the final path component is deliberately not O_NOFOLLOW.
	if info, err := os.Stat(path); err != nil {
		return "", err
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Re-check on the opened descriptor so the validated file is the one read, even
	// if the path was swapped after the stat above.
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if !tokenFileModeAcceptable(info.Mode()) {
		return "", fmt.Errorf("%s has unsafe permissions (%#o); restrict it to the owner, allowing at most group read (e.g. 0600, 0640 or 0440)", path, info.Mode().Perm())
	}

	data, err := io.ReadAll(io.LimitReader(f, maxRegistrationTokenFileSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxRegistrationTokenFileSize {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxRegistrationTokenFileSize)
	}

	// Trim exactly one trailing line ending (as written by editors or `echo`); any
	// other whitespace is left alone so the token is never silently altered.
	token := strings.TrimSuffix(string(data), "\n")
	token = strings.TrimSuffix(token, "\r")
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// tokenFileModeAcceptable mirrors the credential store's policy for files it cannot
// tighten itself: no access at all for "other" and no write for "group", while group
// read is allowed because a Kubernetes Secret volume is readable by a non-root
// process only through the pod's fsGroup.
func tokenFileModeAcceptable(mode os.FileMode) bool {
	// 0o027 = group-write (0o020) plus all "other" bits (0o007).
	return mode.Perm()&0o027 == 0
}
