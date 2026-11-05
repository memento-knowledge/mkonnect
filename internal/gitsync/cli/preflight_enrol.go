package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/memento-knowledge/mkonnect/internal/auth"
	"github.com/memento-knowledge/mkonnect/internal/config"
	"github.com/memento-knowledge/mkonnect/internal/gitsync"
)

// The checks in this file cover what the connector needs to register with the gateway: its
// connector ID, the signing key it keeps once registered, and the one-time registration token it
// presents before then. Nothing here contacts the platform. A registration token is never read for
// its value: its presence, readability and file mode are all that is judged, and no detail or remedy
// carries the token, the path it came from, or any raw error.

// checkConnectorID reports whether CONNECTOR_ID is set. The daemon refuses to start without it, and
// it has no default: the API connector's ID must not be reused. The value is not shown.
func checkConnectorID() checkResult {
	r := checkResult{ID: "connector-id"}
	if os.Getenv("CONNECTOR_ID") == "" {
		r.Status = statusFail
		r.Detail = "CONNECTOR_ID is not set: the connector would not start"
		r.Remedy = "set CONNECTOR_ID to the connector ID issued for the git-sync connector (not the API connector's), copied exactly from your installation instructions"
		return r
	}
	r.Status = statusPass
	r.Detail = "CONNECTOR_ID is set (its value is not checked here: the gateway judges it at registration)"
	return r
}

// enrolment is what the key file says about whether the connector has registered.
type enrolment int

const (
	notEnrolled      enrolment = iota // no key file: the connector will register
	enrolled                          // a key file exists
	enrolmentUnknown                  // the key file could not be judged, so neither is known
)

// checkKeyFile judges the signing key's place, with the daemon's own path rule and key store. It also
// reports whether the connector is already enrolled, which is when a key file exists: the registration
// token is needed only before that. A key that cannot be read or parsed leaves enrolment unknown.
//
// With a key present, the daemon's loader gives the verdict: it refuses a key accessible by anyone but
// its owner, so this fails closed on a loose mode rather than tightening it (it may be running as
// another user than the connector). Without one, the connector must be able to write the key where it
// will be saved, so the daemon's own probe (a file of the key's size, created and removed) is run; the
// key directory is not created here, for the same reason the temporary directory check does not
// create its directory.
func checkKeyFile() (res checkResult, state enrolment) {
	r := checkResult{ID: "key-file"}
	path, err := gitsync.KeyFilePath()
	if err != nil {
		r.Status = statusFail
		r.Detail = gitsync.KeyFileEnv + " is not set and there is no home directory to default it from: the connector would not start"
		r.Remedy = "set " + gitsync.KeyFileEnv + " to the absolute path of the signing key on a persistent writable volume (the git-sync image uses /data/gitsync-key)"
		return r, enrolmentUnknown
	}
	ks := auth.NewKeyStore(path)
	_, found, err := ks.Load()
	switch {
	case err == nil && found:
		r.Status = statusPass
		r.Detail = fmt.Sprintf("%s holds the signing key, and only its owner can access it: this connector is already enrolled", path)
		if !filepath.IsAbs(path) {
			r.Status = statusWarn
			r.Detail += fmt.Sprintf("; %s is a relative path, resolved against the working directory of whatever runs this check, which may not be the connector's", gitsync.KeyFileEnv)
			r.Remedy = "set " + gitsync.KeyFileEnv + " to an absolute path"
		}
		if owner, ok := fileOwner(path); ok && owner != os.Geteuid() {
			r.Status = statusWarn
			r.Detail = fmt.Sprintf("%s holds the signing key (this connector is already enrolled), but it is owned by uid %d, not by uid %d, who ran this check; the connector can read it only if it runs as its owner", path, owner, os.Geteuid())
			r.Remedy = "run preflight as the user the connector runs as; a result for another user, root included, does not show what the connector will see"
		}
		return r, enrolled
	case errors.Is(err, auth.ErrUnsafeKeyPermissions):
		// Do not delete a key to get past this: it cannot be recreated, only registered again.
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s exists but other users can access it: the connector refuses a signing key that is not owner-only, and cannot connect", path)
		r.Remedy = "run `chmod 600` on it as its owner and do not delete it (see \"Restoring the key file mode\" in the git-sync guide), then run preflight again; preflight does not change it"
		return r, enrolled
	case errors.Is(err, syscall.ENOTDIR):
		// A path component is a file: no key can exist there, and none can be saved.
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s cannot exist: a directory in its path is a file, so no signing key can be saved there", path)
		r.Remedy = "check " + gitsync.KeyFileEnv + " for a typo, and point it at a file inside a directory on a persistent writable volume"
		return r, notEnrolled
	case errors.Is(err, fs.ErrPermission):
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s cannot be read by uid %d, so whether the connector is enrolled is not known", path, os.Geteuid())
		r.Remedy = "run preflight as the user the connector runs as, or fix the file's owner (the key must be owned by that user, mode 0600)"
		return r, enrolmentUnknown
	case err != nil:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s cannot be used as a signing key (it is not a regular file, is a symbolic link, is damaged, or cannot be read), so whether the connector is enrolled is not known", path)
		r.Remedy = "restore the key from a backup of the volume; if it is lost, the connector must be registered again with a new registration token from the portal. Do not delete a key that may still be valid"
		return r, enrolmentUnknown
	}

	// No key yet: the connector will register and must be able to save one.
	dir := filepath.Dir(path)
	switch info, err := os.Stat(dir); {
	case err == nil && !info.IsDir():
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s is not a directory, so the signing key cannot be saved at %s", dir, path)
		r.Remedy = "point " + gitsync.KeyFileEnv + " at a file inside a directory on a persistent writable volume"
	case err == nil:
		r.Status, r.Detail, r.Remedy = keyDirProbe(ks, dir)
	case errors.Is(err, fs.ErrNotExist) && gitsync.CanCreateDir(dir):
		// The connector creates the directory (mode 0700) when it registers; preflight does not, because it
		// may be running as another user than the connector.
		r.Status = statusWarn
		r.Detail = fmt.Sprintf("%s does not exist yet; the connector creates it, owner-only, when it registers. Whether the key can be saved there, and the free space, were not tested", dir)
		r.Remedy = "create it as the user the connector runs as (mode 0700) and run preflight again to test it"
	case errors.Is(err, fs.ErrNotExist):
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s does not exist, and the connector's user cannot create it, so registration would fail and spend the registration token", dir)
		r.Remedy = "mount a writable persistent volume there (a read-only root file system cannot create it), or point " + gitsync.KeyFileEnv + " at a path on one"
	default:
		r.Status = statusFail
		r.Detail = fmt.Sprintf("%s cannot be inspected: %s", dir, fsErrorReason(err))
		r.Remedy = "check the volume mount and its permissions for the user the connector runs as"
	}
	return r, notEnrolled
}

// fileOwner returns the uid that owns path.
func fileOwner(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// keyDirProbe runs the daemon's own writability probe on an existing key directory and turns its
// outcome into a status, detail and remedy. A device with no room is told apart because the fix
// differs: the probe writes as many bytes as the key has.
func keyDirProbe(ks *auth.KeyStore, dir string) (checkStatus, string, string) {
	return keyProbeVerdict(ks.ProbeWritable(), dir)
}

// keyProbeVerdict is the verdict for the outcome of the probe.
func keyProbeVerdict(err error, dir string) (checkStatus, string, string) {
	switch {
	case err == nil:
		return statusPass, fmt.Sprintf("not enrolled yet; %s is writable and has room for the signing key (a probe file of the key's size was written and removed)", dir), ""
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return statusFail, fmt.Sprintf("%s has no free space for the signing key", dir),
			"free space on that volume, or give it more; registration spends the one-time registration token and needs to save the key"
	case errors.Is(err, syscall.EROFS):
		return statusFail, fmt.Sprintf("%s is on a read-only file system, so the signing key cannot be saved", dir),
			"mount the data volume read-write, for this check as well as for the connector (a `:ro` mount cannot be used for the connector's own start)"
	default:
		return statusFail, fmt.Sprintf("%s is not writable by uid %d, so registration would fail and spend the registration token", dir, os.Geteuid()),
			"make the directory writable by the user the connector runs as (for Docker, a volume mounted at /data; for Kubernetes, the pod's fsGroup), and run preflight again"
	}
}

// checkRegistrationToken judges where the connector would get its one-time registration token. It
// applies the daemon's own rules to REGISTRATION_TOKEN_FILE (absolute path, regular file, at most
// group read, within the size limit, not empty), which the daemon enforces at every start, so a
// bad file fails even for a connector that is already enrolled, and whether or not the key could be
// judged. A key that cannot be judged leaves enrolment unknown: a usable setting is then skipped,
// because whether a token is needed is not known. Whether the token is accepted is
// the gateway's to say at registration and is never asked here.
func checkRegistrationToken(state enrolment) checkResult {
	r := checkResult{ID: "registration-token"}
	src, err := config.CheckRegistrationToken()
	switch {
	case err != nil:
		// The daemon's error carries the path, which came from the environment, so a fixed text is
		// shown instead of it.
		r.Status = statusFail
		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.Detail = "REGISTRATION_TOKEN_FILE names a file that does not exist: the connector would not start"
			r.Remedy = "mount the file (on Kubernetes, check the Secret and its key), or unset REGISTRATION_TOKEN_FILE if the connector is already enrolled"
		case errors.Is(err, fs.ErrPermission):
			r.Detail = fmt.Sprintf("REGISTRATION_TOKEN_FILE cannot be read by uid %d: the connector would not start", os.Geteuid())
			r.Remedy = "make the file readable by the user the connector runs as, with no access for others and no write for its group (mode 0600, 0640 or 0440)"
		default:
			r.Detail = "REGISTRATION_TOKEN_FILE is not usable: the connector would not start"
			r.Remedy = "it must be an absolute path to a regular file of at most 4096 bytes that is not empty, readable only by its owner (mode 0600), or also by its group (0640 or 0440): no access for others and no write for the group"
		}
	case state == enrolmentUnknown:
		// The file is sound, but whether it is needed depends on a key that could not be judged.
		r.Status = statusSkip
		r.Detail = "not fully checked: whether the connector is enrolled is not known (see key-file), so whether a token is needed is not known; the token settings themselves are usable"
	case state == enrolled && src == config.RegistrationTokenNone:
		r.Status = statusPass
		r.Detail = "no registration token is configured, and none is needed: the connector is already enrolled"
	case state == enrolled:
		r.Status = statusWarn
		r.Detail = "a registration token is still configured, but the connector is already enrolled and no longer needs it"
		r.Remedy = "remove it (see \"Remove the registration token\" in the git-sync guide), so a spent credential is not left on the host"
	case src == config.RegistrationTokenFile:
		r.Status = statusPass
		r.Detail = "REGISTRATION_TOKEN_FILE is a usable file with a safe mode (its content is neither shown nor sent anywhere)"
		if os.Getenv("REGISTRATION_TOKEN") != "" {
			r.Detail += "; REGISTRATION_TOKEN is also set, and is ignored while the file is"
		}
	case src == config.RegistrationTokenEnv:
		r.Status = statusWarn
		r.Detail = "REGISTRATION_TOKEN is set in the environment (its value is neither shown nor sent anywhere)"
		r.Remedy = "prefer REGISTRATION_TOKEN_FILE with a mounted file: an environment variable is visible in `docker inspect` and in the Kubernetes pod specification"
	default:
		r.Status = statusFail
		r.Detail = "no registration token is configured, and the connector is not enrolled (no key file), so it could not register"
		r.Remedy = "set REGISTRATION_TOKEN_FILE to the file holding the token from your installation instructions; the connector needs it only for its first start"
	}
	return r
}
