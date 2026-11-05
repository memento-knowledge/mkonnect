package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// gitVersionTimeout bounds `git --version`.
const gitVersionTimeout = 5 * time.Second

// checkGit runs `git --version` as the connector does and requires 2.31 or newer, the oldest git
// with the GIT_CONFIG_COUNT credential injection the connector relies on.
func checkGit(ctx context.Context) checkResult {
	r := checkResult{ID: "git"}
	path, err := exec.LookPath("git")
	if err != nil {
		r.Status = statusFail
		r.Detail = "git was not found on PATH"
		r.Remedy = "use the mkonnect-gitsync image, which includes git, or install git 2.31 or newer in the environment that runs the connector"
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, gitVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s did not run `--version` successfully", path)
		r.Remedy = "reinstall git, or check that the binary is executable by the user running the connector"
		return r
	}
	ver, major, minor, ok := gitsync.ParseGitVersion(string(out))
	switch {
	case !ok:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s printed output that is not a git version", path)
		r.Remedy = "check that `git` on PATH is the real git binary"
	case !gitsync.GitVersionSupported(major, minor):
		r.Status = statusFail
		r.Detail = fmt.Sprintf("git %s at %s is older than the required %d.%d", ver, path, gitsync.MinGitMajor, gitsync.MinGitMinor)
		r.Remedy = fmt.Sprintf("install git %d.%d or newer (the mkonnect-gitsync image pins a supported version)", gitsync.MinGitMajor, gitsync.MinGitMinor)
	default:
		r.Status = statusPass
		r.Detail = fmt.Sprintf("git %s at %s (%d.%d or newer required)", ver, path, gitsync.MinGitMajor, gitsync.MinGitMinor)
	}
	return r
}

// checkTempDir resolves the temp root with the connector's own rules and proves it is writable by
// creating a directory and a file in it and removing both. It never creates the root itself: a
// preflight run as another user than the connector's would leave a directory the connector cannot
// use.
func checkTempDir() checkResult {
	r := checkResult{ID: "tmpdir"}
	root, source, err := gitsync.TempRootPath()
	if err != nil {
		r.Status = statusFail
		r.Detail = source + " must be an absolute path"
		r.Remedy = "set GIT_SYNC_TMP_DIR (or TMPDIR) to the absolute path of a writable directory"
		return r
	}
	switch err := gitsync.CheckTempRootWritable(root); {
	case err == nil:
		r.Status = statusPass
		r.Detail = fmt.Sprintf("%s (from %s) is writable; the probe was removed", root, source)
	case errors.Is(err, fs.ErrNotExist) && gitsync.CanCreateDir(root):
		// The connector creates the root (mode 0700) at startup, so this host would work; preflight
		// does not, because it may be running as another user than the connector.
		r.Status = statusWarn
		r.Detail = fmt.Sprintf("%s (from %s) does not exist yet; the connector creates it at startup. Its writability was not tested, so the origins were not checked", root, source)
		r.Remedy = "create it as the user the connector runs as (mode 0700) and run preflight again to test it"
	case errors.Is(err, fs.ErrNotExist):
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s (from %s) does not exist, and the connector's user cannot create it", root, source)
		r.Remedy = "mount a writable volume there (a read-only root file system cannot create it), or point GIT_SYNC_TMP_DIR at an existing writable directory"
	default:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s (from %s) is not usable: %s", root, source, fsErrorReason(err))
		r.Remedy = "point GIT_SYNC_TMP_DIR (or TMPDIR) at a writable directory, for example a volume mounted for scratch space"
	}
	return r
}

// fsErrorReason names a file system error without the path it carries.
func fsErrorReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return err.Error()
}

// checkReplicaHost applies the daemon's own parser to GIT_SYNC_REPLICA_HOST. Without a valid pin the
// connector fails closed: it connects but rejects every configuration and never syncs. A value that
// is invalid is never echoed.
func checkReplicaHost() checkResult {
	r := checkResult{ID: "replica-host"}
	host, err := gitsync.ReplicaHostFromEnv()
	switch {
	case err == nil:
		r.Status = statusPass
		r.Detail = fmt.Sprintf("%s=%s; the connector pushes only to this host. Confirm it is the replica host from your installation instructions.", gitsync.ReplicaHostEnv, host)
	case errors.Is(err, gitsync.ErrReplicaHostNotSet):
		r.Status = statusFail
		r.Detail = gitsync.ReplicaHostEnv + " is not set: the connector would reject every configuration and not sync"
		r.Remedy = "set " + gitsync.ReplicaHostEnv + " to the replica host from your installation instructions (host or host:port, no https://), and restart the connector after changing it"
	default:
		r.Status = statusFail
		r.Detail = err.Error() + ": the connector would reject every configuration and not sync"
		r.Remedy = "set " + gitsync.ReplicaHostEnv + " to the replica host only: host or host:port, no scheme, path or spaces"
	}
	return r
}

// checkProviderConfig applies the verdict `config check` gives to the provider configuration the
// connector would load.
func checkProviderConfig(rep gitsync.ConfigReport) checkResult {
	r := checkResult{ID: "provider-config"}
	switch {
	case rep.Missing && rep.Path != gitsync.DefaultConfigFile:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s does not exist (from GIT_SYNC_CONFIG_FILE)", rep.Path)
		r.Remedy = "check GIT_SYNC_CONFIG_FILE, or unset it to use the default " + gitsync.DefaultConfigFile
	case len(rep.Issues) > 0:
		n := len(rep.Issues)
		lines := []string{fmt.Sprintf("the connector would refuse %s: %d %s", rep.Path, n, plural(n, "problem", "problems"))}
		for _, issue := range rep.Issues {
			lines = append(lines, "  "+describeIssue(issue))
		}
		r.Status = statusFail
		r.Detail = strings.Join(lines, "\n")
		r.Remedy = "correct the file, or run `connector config check` for the same report"
	case rep.Missing:
		r.Status = statusWarn
		r.Detail = fmt.Sprintf("%s does not exist: the connector starts with no providers, so every repository is rejected as not host-matched", rep.Path)
		r.Remedy = "add your git server with `connector config set <name> --base-url <url> --auth bearer --token-stdin`; the connector picks it up without a restart"
	case len(rep.Warnings) > 0:
		r.Status = statusWarn
		r.Detail = fmt.Sprintf("%s is accepted, with %s:\n  %s", rep.Path, plural(len(rep.Warnings), "a warning", "warnings"), strings.Join(rep.Warnings, "\n  "))
		r.Remedy = "see `connector config check` and the git-sync guide"
	default:
		r.Status = statusPass
		n := len(rep.Providers)
		r.Detail = fmt.Sprintf("%s is accepted: %d %s", rep.Path, n, plural(n, "provider", "providers"))
	}
	return r
}

// checkOrigins probes each configured origin with the connector's own prober. The credential of a
// provider goes only to that provider's base URL; it is not sent to any other host, and redirects
// are not followed.
func checkOrigins(ctx context.Context, prober gitsync.OriginProber, cfg gitsync.LoadedConfig) []checkResult {
	if len(cfg.Report.Providers) == 0 {
		return []checkResult{{
			ID:     "origins",
			Status: statusWarn,
			Detail: "no providers are configured, so no git server was checked",
			Remedy: "add your git server with `connector config set <name> --base-url <url> --auth bearer --token-stdin`, then run preflight again",
		}}
	}
	var out []checkResult
	for _, o := range cfg.ProbeOrigins(ctx, prober) {
		r := checkResult{ID: "origin:" + o.Name}
		who := o.HostKey
		switch {
		case ctx.Err() != nil && (o.Err != nil || o.Result == gitsync.OriginUnreachable):
			// The probe was cut short by the command's own time limit: that says nothing about the origin.
			r.Status = statusSkip
			r.Detail = fmt.Sprintf("%s: not checked, because the time limit for this command ran out", who)
		case o.Err != nil:
			r.Status = statusFail
			r.Detail = fmt.Sprintf("%s: the check could not run: %s", who, localFault(o.Err))
			r.Remedy = "fix the problem named above (see the git and tmpdir checks), then run preflight again"
		case o.Result == gitsync.OriginReachable && o.HasCredential:
			r.Status = statusPass
			r.Detail = fmt.Sprintf("%s answered. The credential is verified only if the server asked for one; a repository the credential cannot read is reported at its first sync.", who)
		case o.Result == gitsync.OriginReachable:
			r.Status = statusPass
			r.Detail = fmt.Sprintf("%s answered, with no credential configured for it", who)
		case o.Result == gitsync.OriginAccessDenied && o.HasCredential:
			r.Status = statusFail
			r.Detail = fmt.Sprintf("%s answered but rejected the credential", who)
			r.Remedy = "check the token's expiry and permissions, then store a working one with `connector config set " + o.Name + " ...`; the connector picks it up without a restart"
		case o.Result == gitsync.OriginAccessDenied:
			r.Status = statusFail
			r.Detail = fmt.Sprintf("%s answered but requires authentication, and no credential is configured for it", who)
			r.Remedy = "store a credential with `connector config set " + o.Name + " --base-url ... --auth bearer --token-stdin`"
		default:
			r.Status = statusFail
			r.Detail = fmt.Sprintf("%s could not be reached (DNS, connection, proxy or TLS)", who)
			r.Remedy = "from the host that runs the connector, check DNS, the firewall or network policy, the proxy settings (HTTPS_PROXY, NO_PROXY) and, for an internal CA, GIT_SSL_CAINFO"
		}
		out = append(out, r)
	}
	return out
}

// localFault renders a fault of the probe itself. These come from starting git or building its
// environment and carry no credential.
func localFault(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) {
		return "git could not be started"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s: %s", pe.Op, pe.Err)
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission denied"
	}
	return "an internal error occurred"
}
