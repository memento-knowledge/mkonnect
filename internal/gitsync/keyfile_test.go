package gitsync_test

import (
	"path/filepath"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

func TestKeyFilePath(t *testing.T) {
	t.Run("KEY_FILE wins, verbatim", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("KEY_FILE", "relative/key")
		got, err := gitsync.KeyFilePath()
		if err != nil || got != "relative/key" {
			t.Errorf("got %q, %v", got, err)
		}
	})
	t.Run("default is distinct from the API connector's", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("KEY_FILE", "")
		got, err := gitsync.KeyFilePath()
		if want := filepath.Join(home, ".mkonnect", "gitsync-key"); err != nil || got != want {
			t.Errorf("got %q, %v, want %q", got, err, want)
		}
	})
	t.Run("no home", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("KEY_FILE", "")
		if _, err := gitsync.KeyFilePath(); err == nil {
			t.Error("expected an error with neither KEY_FILE nor a home directory")
		}
	})
}
