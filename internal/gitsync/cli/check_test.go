package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// checkToken is a synthetic credential the check output must never contain.
const checkToken = "check-token-0123456789abcdef"

// runCheck writes content (when non-empty) to a fresh config file at mode, runs `config check`, and
// returns the output with the temp path replaced by <config> so it can be compared to a golden file.
func runCheck(t *testing.T, content string, mode os.FileMode) (string, error) {
	t.Helper()
	path := withConfig(t)
	if content != "" {
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runCLI(t, "", "config", "check")
	out = strings.ReplaceAll(out, path, "<config>")
	return uidRE.ReplaceAllString(out, "uid N"), err
}

var uidRE = regexp.MustCompile(`uid \d+`)

func TestConfigCheck(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		wantErr string // substring of the returned error; "" means success
	}{
		{
			name: "check_valid",
			content: `{
  "gitea": {"base_url": "https://git.example.test:8443/", "auth": "basic", "username": "svc", "token": "` + checkToken + `"},
  "origin": {"base_url": "https://Git.Example.Test", "auth": "bearer", "token": "` + checkToken + `"},
  "public": {"base_url": "http://mirror.example.test/"}
}`,
			mode: 0o600,
		},
		{name: "check_missing", wantErr: "does not exist"},
		{name: "check_empty_config", content: "{}", mode: 0o600},
		{
			// Group read is what a mounted Kubernetes Secret has: no warning.
			name:    "check_group_readable",
			content: `{"origin": {"base_url": "https://git.example.test/"}}`,
			mode:    0o640,
		},
		{
			name:    "check_world_readable",
			content: `{"origin": {"base_url": "https://git.example.test/", "auth": "bearer", "token": "` + checkToken + `"}}`,
			mode:    0o644,
		},
		{
			name: "check_every_error",
			content: `{
  "bad name!": {"base_url": "https://one.example.test/"},
  "alpha": {"base_url": "ftp://alpha.example.test/"},
  "beta": {"base_url": "https://beta.example.test/", "auth": "bearer", "token": "short"},
  "gamma": {"base_url": "http://gamma.example.test/", "auth": "bearer", "token": "` + checkToken + `"},
  "delta": {"base_url": "https://delta.example.test/", "auth": "basic", "token": "` + checkToken + `"},
  "epsilon": {"base_url": "https://delta.example.test:443/path"},
  "zeta": {"base_url": "https://zeta.example.test/", "token": "` + checkToken + `"},
  "eta": {"base_url": "https://eta.example.test/", "auth": "kerberos"},
  "theta": {"base_url": "https://theta.example.test/", "auth": "bearer", "username": "u", "token": "` + checkToken + `"}
}`,
			mode:    0o600,
			wantErr: "found 8 problems",
		},
		{
			name: "check_duplicate_host",
			content: `{
  "one": {"base_url": "https://git.example.test/team"},
  "two": {"base_url": "https://GIT.example.test:443/other"}
}`,
			mode:    0o600,
			wantErr: "found 1 problem",
		},
		{
			name:    "check_syntax_error",
			content: `{"origin": {"base_url": "https://git.example.test/", "token": "` + checkToken + `"},`,
			mode:    0o600,
			wantErr: "found 1 problem",
		},
		{
			name:    "check_unknown_field",
			content: `{"origin": {"base_url": "https://git.example.test/", "` + checkToken + `": "x"}}`,
			mode:    0o600,
			wantErr: "found 1 problem",
		},
		{
			name:    "check_wrong_type",
			content: `{"origin": {"base_url": "https://git.example.test/", "token": 12345678901234567890}}`,
			mode:    0o600,
			wantErr: "found 1 problem",
		},
		{
			name:    "check_trailing_data",
			content: `{"origin": {"base_url": "https://git.example.test/"}} ` + checkToken,
			mode:    0o600,
			wantErr: "found 1 problem",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runCheck(t, tt.content, tt.mode)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("config check failed: %v\n%s", err, out)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("config check error = %v, want one containing %q\n%s", err, tt.wantErr, out)
			}
			for _, leak := range []string{checkToken, "12345678901234567890", "bad name"} {
				if strings.Contains(out, leak) || (err != nil && strings.Contains(err.Error(), leak)) {
					t.Errorf("config check output or error repeats %q:\n%s\n%v", leak, out, err)
				}
			}
			goldenCompare(t, tt.name+".golden", out)
		})
	}
}

// The file is read from GIT_SYNC_CONFIG_FILE, as everywhere else, and the output says which file and
// why. There is no path flag to disagree with the connector's.
func TestConfigCheckUsesTheConfiguredPathAndTakesNoArguments(t *testing.T) {
	out, err := runCheck(t, `{}`, 0o600)
	if err != nil || !strings.Contains(out, "File: <config> (from GIT_SYNC_CONFIG_FILE)") {
		t.Fatalf("err=%v, output:\n%s", err, out)
	}

	t.Setenv("GIT_SYNC_CONFIG_FILE", "")
	out, err = runCLI(t, "", "config", "check")
	if err != nil || !strings.Contains(out, "File: /data/git-sync.json (the default; GIT_SYNC_CONFIG_FILE is not set)") {
		t.Fatalf("default path: err=%v, output:\n%s", err, out)
	}

	withConfig(t)
	if _, err := runCLI(t, "", "config", "check", "other.json"); err == nil || !strings.Contains(err.Error(), "GIT_SYNC_CONFIG_FILE") {
		t.Errorf("an argument must be refused with a pointer to the variable, got %v", err)
	}
	if out, err := runCLI(t, "", "config", "check", "-h"); err != nil || !strings.Contains(out, "usage: mkonnect-gitsync connector config check") {
		t.Errorf("-h: err=%v, output:\n%s", err, out)
	}
}

// check reads the same file the same way as `config list`: a file that loads for one loads for the
// other, and one that does not fails both.
func TestConfigCheckAgreesWithTheLoader(t *testing.T) {
	good := `{"origin": {"base_url": "https://git.example.test/", "auth": "bearer", "token": "` + checkToken + `"}}`
	bad := `{"origin": {"base_url": "https://git.example.test/", "auth": "bearer", "token": "short"}}`
	for content, wantFail := range map[string]bool{good: false, bad: true} {
		path := withConfig(t)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, checkErr := runCLI(t, "", "config", "check")
		_, listErr := runCLI(t, "", "config", "list")
		if (checkErr != nil) != wantFail || (listErr != nil) != wantFail {
			t.Errorf("check err = %v, list err = %v, want failure=%v for both", checkErr, listErr, wantFail)
		}
	}
}

// tokenFileCase is one `config check` run against a config that names token files. files are written
// next to the config (name -> content and mode); the config text uses {dir} for that directory.
type tokenFileCase struct {
	name    string
	config  string
	files   map[string]checkFile
	wantErr string
}

type checkFile struct {
	content string
	mode    os.FileMode
}

func TestConfigCheckTokenFiles(t *testing.T) {
	const ok = checkToken + "\n"
	tests := []tokenFileCase{
		{
			// The credential source is reported, neither the path nor the content.
			name: "check_token_file_valid",
			config: `{
  "filed": {"base_url": "https://git.example.test/", "auth": "bearer", "token_file": "{dir}/good"},
  "inline": {"base_url": "https://git-b.example.test/", "auth": "bearer", "token": "` + checkToken + `"}
}`,
			files: map[string]checkFile{"good": {ok, 0o600}},
		},
		{
			// One problem per class of token-file failure, each located by provider and field.
			name: "check_token_file_errors",
			config: `{
  "a-missing": {"base_url": "https://a.example.test/", "auth": "bearer", "token_file": "{dir}/absent"},
  "b-writable": {"base_url": "https://b.example.test/", "auth": "bearer", "token_file": "{dir}/writable"},
  "c-crlf": {"base_url": "https://c.example.test/", "auth": "bearer", "token_file": "{dir}/crlf"},
  "d-empty": {"base_url": "https://d.example.test/", "auth": "bearer", "token_file": "{dir}/empty"},
  "e-short": {"base_url": "https://e.example.test/", "auth": "bearer", "token_file": "{dir}/short"},
  "f-fine": {"base_url": "https://f.example.test/", "auth": "bearer", "token_file": "{dir}/good"}
}`,
			files: map[string]checkFile{
				"writable": {ok, 0o666},
				"crlf":     {checkToken + "\r\n", 0o600},
				"empty":    {"", 0o600},
				"short":    {"short\n", 0o600},
				"good":     {ok, 0o600},
			},
			wantErr: "found 5 problems",
		},
		{
			name:    "check_token_file_empty_value",
			config:  `{"p": {"base_url": "https://git.example.test/", "auth": "bearer", "token_file": ""}}`,
			wantErr: "found 1 problem",
		},
		{
			name:    "check_token_file_and_token",
			config:  `{"p": {"base_url": "https://git.example.test/", "auth": "bearer", "token": "` + checkToken + `", "token_file": "{dir}/good"}}`,
			files:   map[string]checkFile{"good": {ok, 0o600}},
			wantErr: "found 1 problem",
		},
		{
			// A configuration with any other problem opens no token file, so the missing file is not
			// reported until the duplicate host is fixed.
			name: "check_token_file_skipped_when_another_problem",
			config: `{
  "one": {"base_url": "https://git.example.test/", "auth": "bearer", "token_file": "{dir}/absent"},
  "two": {"base_url": "https://git.example.test/other"}
}`,
			wantErr: "found 1 problem",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := withConfig(t)
			dir := filepath.Dir(path)
			for name, f := range tt.files {
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, []byte(f.content), f.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p, f.mode); err != nil {
					t.Fatal(err)
				}
			}
			cfg := strings.ReplaceAll(tt.config, "{dir}", dir)
			if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runCLI(t, "", "config", "check")
			out = strings.ReplaceAll(out, path, "<config>")
			out = strings.ReplaceAll(out, dir, "<dir>")
			out = uidRE.ReplaceAllString(out, "uid N")

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("config check failed: %v\n%s", err, out)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("config check error = %v, want one containing %q\n%s", err, tt.wantErr, out)
			}
			// Never the credential (in the file, with or without its line ending) and, for a config that
			// is accepted, never a path either.
			for _, leak := range []string{checkToken, strings.TrimSuffix(checkToken, checkToken[len(checkToken)-4:])} {
				if strings.Contains(out, leak) || (err != nil && strings.Contains(err.Error(), leak)) {
					t.Errorf("config check output or error repeats the credential:\n%s\n%v", out, err)
				}
			}
			goldenCompare(t, tt.name+".golden", out)
		})
	}
}
