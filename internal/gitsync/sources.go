package gitsync

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Source is one file the loader read, or tried to read, while loading the provider configuration:
// the config file itself and every token file it names. A caller that reloads the configuration
// compares Sources from successive loads (they are comparable with ==) to learn whether anything
// that fed the configuration changed.
//
// The comparison covers the bytes the loader actually read, through the same vetted descriptor that
// produced the configuration, so a file replaced after that read always compares as changed; a
// fingerprint taken by re-reading the path later would race with the load and could hide it. The
// fingerprint is an HMAC-SHA256 of the raw file bytes under a random per-process key, kept in memory
// only: it is not exported, and String and GoString omit it, so formatting or logging a Source (with
// %v, %+v or %#v) cannot disclose anything about a credential, and fingerprints from different
// processes are not comparable.
type Source struct {
	// Path is the path as configured (not resolved through symlinks).
	Path string
	// Read reports that the file existed and was read. A false value means it was absent or could
	// not be used; a reload that sees it become true has something new to load.
	Read   bool
	digest [sha256.Size]byte
}

// digestKey keys the fingerprints with a per-process random value, so one is only meaningful inside
// this process and, even if a reflection-based dumper prints it, cannot be tested against guesses
// of a credential offline.
var digestKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("gitsync: no randomness for source fingerprints: " + err.Error())
	}
	return k
}()

func newSource(path string, content []byte) Source {
	mac := hmac.New(sha256.New, digestKey)
	mac.Write(content)
	s := Source{Path: path, Read: true}
	copy(s.digest[:], mac.Sum(nil))
	return s
}

// String describes the source without its fingerprint.
func (s Source) String() string {
	if s.Read {
		return s.Path
	}
	return s.Path + " (not read)"
}

// GoString keeps the fingerprint out of %#v output too (which would otherwise print every field,
// and which a *LoadError or []Source also reaches).
func (s Source) GoString() string { return "gitsync.Source{" + s.String() + "}" }

// LoadError is returned by New for any failure after the config file was located. It wraps the
// cause unchanged (so Error and errors.Is behave as without it) and carries the files the loader
// had gathered when it gave up, so a caller retrying a failed load knows which files, including a
// token file that does not exist yet, to watch.
type LoadError struct {
	// Sources are every file involved, sorted by path.
	Sources []Source
	err     error
}

func (e *LoadError) Error() string { return e.err.Error() }
func (e *LoadError) Unwrap() error { return e.err }

// sourceSet collects the files of one load, keyed by path.
type sourceSet map[string]Source

func (m sourceSet) add(s Source) { m[s.Path] = s }

// sorted returns the sources ordered by path.
func (m sourceSet) sorted() []Source {
	out := make([]Source, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Source) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// maxTokenFilePathLength bounds a configured token_file path.
const maxTokenFilePathLength = 4096

// validateTokenFilePath checks a token_file path from the config. The path is printed in errors and
// logs, so it must be absolute (the daemon's working directory cannot be relied on) and free of
// control and invisible characters. It never includes the path in its own error, since the path is
// what failed.
func validateTokenFilePath(path string) error {
	if len(path) > maxTokenFilePathLength {
		return fmt.Errorf("token_file must be at most %d bytes", maxTokenFilePathLength)
	}
	if !filepath.IsAbs(path) {
		return errors.New("token_file must be an absolute path")
	}
	if !utf8.ValidString(path) || strings.ContainsFunc(path, isControlOrFormat) {
		return errors.New("token_file must not contain control or invisible characters")
	}
	return nil
}

func isControlOrFormat(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// tokenFileValue extracts the credential from a token file's bytes: exactly one trailing "\n" is
// removed and nothing else is altered, so a stray CR, extra blank line, space or invisible character
// is reported rather than silently changing the credential. The returned message completes the
// sentence "token_file <path> ..." and never contains any of the file's content.
func tokenFileValue(raw []byte) (string, string) {
	if !utf8.Valid(raw) {
		return "", "is not valid UTF-8"
	}
	s, _ := strings.CutSuffix(string(raw), "\n")
	switch {
	case strings.TrimSpace(s) == "":
		return "", "is empty"
	case strings.HasSuffix(s, "\r"):
		return "", "ends with a carriage return (CRLF line ending); save it with a single LF line ending"
	case strings.ContainsRune(s, '\n'):
		return "", "has more than one line; it must hold only the token, with at most one trailing newline"
	case strings.ContainsFunc(s, isControlOrFormat):
		return "", "contains control or invisible characters (for example a byte-order mark)"
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return "", "has leading or trailing whitespace"
	}
	return s, ""
}
