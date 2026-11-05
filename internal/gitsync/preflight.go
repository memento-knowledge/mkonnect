package gitsync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"golang.org/x/sys/unix"
)

// This file holds the parts of the operator CLI's preflight command that need the package's
// internals: reading the provider config without side effects, probing its origins with the
// connector's own prober, and checking the temp root the way TempRoot does.

// maxPreflightProbes bounds how many origins are probed at once. Each probe is a git subprocess with
// its own temp home, and the configuration holds one provider per origin host, so a handful is
// plenty.
const maxPreflightProbes = 4

// LoadedConfig is the provider config read once, for a command that both reports on it and probes
// the origins it names, so the two cannot see different files.
type LoadedConfig struct {
	// Report is what the load found. It holds no credential.
	Report  ConfigReport
	entries map[string]LocalEntry
}

// ReadConfig reads the provider config at path like CheckConfig, with one difference: it never
// changes the file (CheckConfig tightens a file's mode to 0600 as the connector does at startup).
// A file whose mode is wider than expected is reported in Report.Warnings instead.
func ReadConfig(path string) LoadedConfig {
	warnings := modeWarnings(path)
	res := loadConfigMode(path, false)
	rep := buildReport(path, res, warnings)
	// The connector tightens a loose file at startup where it can, and refuses it where it cannot. Whether
	// it can depends on the owner, the file system, attributes and policy, and the only sure way to find
	// out is to change the file, which preflight does not do. A loose file is therefore reported as a
	// failure, not a warning: preflight does not claim a host is ready when it cannot establish it.
	if res.loose {
		msg := "the file's mode is wider than the connector accepts. The connector tightens it to 0600 at startup where it can and otherwise refuses it, and preflight cannot tell which without changing the file. Restrict it to the owner, allowing at most group read (for example chmod 600), and run preflight again"
		rep.Issues = append(rep.Issues, ConfigIssue{Class: IssueFile, Message: msg, err: errors.New(msg)})
	}
	return LoadedConfig{Report: rep, entries: res.entries}
}

// CanCreateDir reports whether the connector's user could create path as a directory: its nearest
// existing ancestor is a directory it can write to and enter. It creates nothing.
func CanCreateDir(path string) bool {
	for dir := filepath.Dir(filepath.Clean(path)); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			return info.IsDir() && unix.Access(dir, unix.W_OK|unix.X_OK) == nil
		}
		if !os.IsNotExist(err) || dir == filepath.Dir(dir) {
			return false
		}
	}
}

// OriginOutcome is the result of probing one configured provider's origin.
type OriginOutcome struct {
	// Name is the provider name (it passed the provider-name rule) and HostKey its canonical
	// scheme://host:port.
	Name, HostKey string
	// HasCredential says the probe carried the provider's credential.
	HasCredential bool
	// Result is the probe's verdict. It is empty when Err is set.
	Result OriginResult
	// Err is a LOCAL fault (for example git cannot be started), which says nothing about the origin.
	Err error
}

// ProbeOrigins probes every provider of a configuration the connector would accept, with the same
// prober and the same locked-down git environment the daemon uses for its own origin check: the
// credential travels only in a header scoped to the provider's own base URL, and redirects are not
// followed (see credConfig). A configuration with any problem, or no file, is not probed: the
// connector would refuse it, and a refused configuration's token files are never read, so a probe
// would report a missing credential as a rejected one. Outcomes are in provider-name order.
func (c LoadedConfig) ProbeOrigins(ctx context.Context, p OriginProber) []OriginOutcome {
	if c.Report.Missing || len(c.Report.Issues) > 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(c.entries))
	out := make([]OriginOutcome, len(names))
	hostKey := make(map[string]string, len(c.Report.Providers))
	for _, pr := range c.Report.Providers {
		hostKey[pr.Name] = pr.HostKey
	}
	sem := make(chan struct{}, maxPreflightProbes)
	var wg sync.WaitGroup
	for i, name := range names {
		e := c.entries[name]
		out[i] = OriginOutcome{Name: name, HostKey: hostKey[name], HasCredential: e.Auth != ""}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := p.Probe(ctx, e)
			out[i].Result, out[i].Err = res, err
		}()
	}
	wg.Wait()
	return out
}

// preflightProbePrefix names the directory CheckTempRootWritable creates. It is the temp-home
// prefix, so the startup sweep removes one a killed command left behind.
const preflightProbePrefix = tempHomePrefix + "preflight-"

// CheckTempRootWritable verifies that root exists and that the connector's user can create a
// directory and a file in it, then removes both, leaving nothing behind. It never creates root.
func CheckTempRootWritable(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	dir, err := os.MkdirTemp(root, preflightProbePrefix)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	f, err := os.OpenFile(filepath.Join(dir, "probe"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("probe"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
