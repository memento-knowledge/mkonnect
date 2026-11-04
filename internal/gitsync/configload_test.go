package gitsync_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

func TestCheckConfigReportsEveryProblemAndNewReportsTheFirst(t *testing.T) {
	path := writeConfig(t, `{
  "zeta": {"base_url": "https://zeta.example.test/", "auth": "bearer", "token": "short"},
  "alpha": {"base_url": "ftp://alpha.example.test/"},
  "beta": {"base_url": "https://beta.example.test/"},
  "beta2": {"base_url": "https://BETA.example.test:443/x"}
}`)
	rep := gitsync.CheckConfig(path)
	type got struct {
		class           gitsync.IssueClass
		provider, field string
	}
	var issues []got
	for _, i := range rep.Issues {
		issues = append(issues, got{i.Class, i.Provider, i.Field})
	}
	want := []got{
		{gitsync.IssueBaseURL, "alpha", "base_url"},
		{gitsync.IssueDuplicateHost, "beta2", "base_url"},
		{gitsync.IssueCredentials, "zeta", "token"},
	}
	if fmt.Sprint(issues) != fmt.Sprint(want) {
		t.Errorf("issues = %v, want %v", issues, want)
	}
	if len(rep.Providers) != 1 || rep.Providers[0].Name != "beta" || rep.Providers[0].HostKey != "https://beta.example.test:443" {
		t.Errorf("valid providers = %+v, want only beta", rep.Providers)
	}

	// New fails with the first of them, the same one every time, and with the same text as before.
	for i := 0; i < 20; i++ {
		_, err := gitsync.New(path)
		if err == nil || err.Error() != `provider "alpha" is invalid: base_url must be an absolute http or https URL` {
			t.Fatalf("New error = %v, want the alpha problem", err)
		}
	}
}

// A key in the file is operator-typed text and may be a pasted secret: no error or issue repeats one,
// whichever way it is wrong.
func TestConfigProblemsNeverEchoKeysOrValues(t *testing.T) {
	canary := newCanary(t, false)
	for name, content := range map[string]string{
		"unknown field key":      `{"origin": {"base_url": "https://git.example.test/", "` + canary + `": "x"}}`,
		"wrong-typed provider":   `{"` + canary + `": "https://git.example.test/"}`,
		"wrong-typed setting":    `{"` + canary + `": {"base_url": 12345}}`,
		"wrong-typed token":      `{"origin": {"base_url": "https://git.example.test/", "token": ` + canary[:0] + `123456789012345678901234567890}}`,
		"array":                  `["` + canary + `"]`,
		"bad provider name":      `{"` + canary + `/x": {"base_url": "https://git.example.test/"}}`,
		"long provider name":     `{"` + strings.Repeat("a", 70) + canary + `": {"base_url": "https://git.example.test/"}}`,
		"url with userinfo":      `{"origin": {"base_url": "https://u:` + canary + `@git.example.test/"}}`,
		"url with query":         `{"origin": {"base_url": "https://git.example.test/?t=` + canary + `"}}`,
		"token on http":          `{"origin": {"base_url": "http://git.example.test/", "auth": "bearer", "token": "` + canary + `"}}`,
		"token with control":     `{"origin": {"base_url": "https://git.example.test/", "auth": "bearer", "token": "` + canary + `\n"}}`,
		"username with colon":    `{"origin": {"base_url": "https://git.example.test/", "auth": "basic", "username": "a:` + canary + `", "token": "` + canary + `"}}`,
		"unknown auth":           `{"origin": {"base_url": "https://git.example.test/", "auth": "` + canary + `"}}`,
		"trailing data":          `{"origin": {"base_url": "https://git.example.test/"}} ` + canary,
		"truncated after secret": `{"origin": {"base_url": "https://git.example.test/", "token": "` + canary + `"},`,
		"garbage":                canary,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			var text []string
			if _, err := gitsync.New(path); err != nil {
				text = append(text, err.Error())
			} else {
				t.Fatal("the config should be refused")
			}
			rep := gitsync.CheckConfig(path)
			if len(rep.Issues) == 0 {
				t.Fatal("check found nothing wrong")
			}
			for _, i := range rep.Issues {
				text = append(text, i.Error(), i.Message, i.Provider, i.Field)
			}
			for _, s := range text {
				if strings.Contains(s, canary) || strings.Contains(s, "123456789012345678901234567890") {
					t.Errorf("a problem repeats the input: %q", s)
				}
			}
		})
	}
}

func TestCheckConfigMissingFileAndFileMode(t *testing.T) {
	rep := gitsync.CheckConfig(filepath.Join(t.TempDir(), "absent.json"))
	if !rep.Missing || len(rep.Issues) != 0 || len(rep.Providers) != 0 {
		t.Errorf("missing file: %+v", rep)
	}

	path := writeConfig(t, `{"origin": {"base_url": "https://git.example.test/"}}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	rep = gitsync.CheckConfig(path)
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "0644") {
		t.Errorf("warnings for a world-readable file = %v", rep.Warnings)
	}
	// Loading tightened it, as the daemon's would.
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode after the check = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	if rep = gitsync.CheckConfig(path); len(rep.Warnings) != 0 {
		t.Errorf("warnings for an owner-only file = %v", rep.Warnings)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if rep = gitsync.CheckConfig(path); len(rep.Warnings) != 0 {
		t.Errorf("warnings for a group-readable file = %v: a mounted Secret has that mode", rep.Warnings)
	}

	if rep = gitsync.CheckConfig(t.TempDir()); len(rep.Issues) != 1 || rep.Issues[0].Class != gitsync.IssueFile {
		t.Errorf("a directory in place of the file: %+v", rep.Issues)
	}
}
