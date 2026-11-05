package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// maxRegistrationTokenFileSize bounds how much of REGISTRATION_TOKEN_FILE is read. A
// registration token is a short opaque string, so anything larger is a misconfigured
// path and is rejected rather than truncated.
const maxRegistrationTokenFileSize = 4096

// RegistrationTokenSource says where the registration token comes from.
type RegistrationTokenSource string

const (
	// RegistrationTokenNone: neither REGISTRATION_TOKEN_FILE nor REGISTRATION_TOKEN is set.
	RegistrationTokenNone RegistrationTokenSource = "none"
	// RegistrationTokenEnv: REGISTRATION_TOKEN, an environment variable.
	RegistrationTokenEnv RegistrationTokenSource = "env"
	// RegistrationTokenFile: the file named by REGISTRATION_TOKEN_FILE.
	RegistrationTokenFile RegistrationTokenSource = "file"
)

// registrationToken resolves the registration token. When REGISTRATION_TOKEN_FILE is
// set the token is read from that file and REGISTRATION_TOKEN is ignored; any failure
// to read the file is an error rather than a silent fallback to the environment
// variable. When it is unset, REGISTRATION_TOKEN is returned verbatim.
func registrationToken() (string, error) {
	token, _, err := resolveRegistrationToken()
	return token, err
}

func resolveRegistrationToken() (string, RegistrationTokenSource, error) {
	path := os.Getenv("REGISTRATION_TOKEN_FILE")
	if path == "" {
		if token := os.Getenv("REGISTRATION_TOKEN"); token != "" {
			return token, RegistrationTokenEnv, nil
		}
		return "", RegistrationTokenNone, nil
	}
	token, err := readRegistrationTokenFile(path)
	if err != nil {
		return "", RegistrationTokenFile, fmt.Errorf("REGISTRATION_TOKEN_FILE: %w", err)
	}
	return token, RegistrationTokenFile, nil
}

// CheckRegistrationToken applies the rules the connector applies to its registration token at
// startup and reports where it comes from, without returning the value, so a caller that only needs
// to know whether the token is usable (the operator preflight) cannot print or log it. The error,
// like Load's, names the problem and the file path but never the contents.
func CheckRegistrationToken() (RegistrationTokenSource, error) {
	_, src, err := resolveRegistrationToken()
	return src, err
}

// readRegistrationTokenFile reads and validates a registration token file. Errors
// describe the problem but never include file contents.
func readRegistrationTokenFile(path string) (string, error) {
	// Like CA_BUNDLE, the path is resolved by a long-running daemon whose working
	// directory a relative path cannot be relied on to match.
	if !filepath.IsAbs(path) {
		return "", errors.New("must be an absolute path")
	}

	// Open without O_NOFOLLOW: a Kubernetes Secret mount exposes each key as a symlink
	// into a ..data directory, so the final path component must be followed. O_NONBLOCK
	// keeps opening a FIFO from blocking, and every check below is made on the opened
	// descriptor, so the file validated is the file read (no stat/open race). This
	// follows the convention of internal/auth/keystore.go.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck

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

	// Trim exactly one trailing line ending, "\n" or "\r\n" (as written by editors or
	// `echo`). A lone trailing "\r" and any other whitespace are left alone so the
	// token is never silently altered.
	token, trimmed := strings.CutSuffix(string(data), "\n")
	if trimmed {
		token = strings.TrimSuffix(token, "\r")
	}
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// tokenFileModeAcceptable applies the same policy as readOnlyModeAcceptable in
// internal/creds, which the credential store uses for files it cannot tighten itself:
// no access at all for "other" and no write for "group", while group read is allowed
// because a Kubernetes Secret volume is readable by a non-root process only through
// the pod's fsGroup.
func tokenFileModeAcceptable(mode os.FileMode) bool {
	// 0o027 = group-write (0o020) plus all "other" bits (0o007).
	return mode.Perm()&0o027 == 0
}
