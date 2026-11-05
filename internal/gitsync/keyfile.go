package gitsync

import (
	"os"
	"path/filepath"
)

// KeyFileEnv names the environment variable that sets the signing key's path.
const KeyFileEnv = "KEY_FILE"

// KeyFilePath returns where the git-sync connector keeps its signing key: KEY_FILE when set, else
// gitsync-key under ~/.mkonnect. The default differs from the API connector's (key), so running
// both with default settings does not make them share one file, where the second registration would
// clobber the first. The daemon and the operator preflight both resolve the path here, so they
// cannot disagree. It fails only when there is no KEY_FILE and no home directory.
func KeyFilePath() (string, error) {
	if p := os.Getenv(KeyFileEnv); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mkonnect", "gitsync-key"), nil
}
