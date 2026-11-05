package gitsync

import "slices"

// binding says which local provider, and which generation of its credential, a repository is bound
// to. The zero value is "unbound": no provider matches the repository's origin host. It is compared
// to decide whether a sync that is running, or has just finished, used the credential the repository
// has now.
//
// gen is a counter, not a digest of the credential. A hash of a token is a verifier an attacker could
// test guesses against offline, and a value that is copied into every repository's state and printed
// by any %+v would carry it into logs. A counter is drawn from one monotonic sequence and never
// reused, so a credential that is replaced and then restored is still a different generation.
type binding struct {
	provider string
	gen      uint64
}

// providerEntry is one provider of a providerSet.
type providerEntry struct {
	entry   LocalEntry
	hostKey string // canonical scheme://host:port of the entry's base URL
	gen     uint64
}

// providerSet is an immutable snapshot of the local provider configuration, indexed by host. The
// connector swaps whole snapshots (see Client.setProviders); nothing mutates one after it is built,
// so a reader that holds one sees a single consistent configuration.
type providerSet struct {
	byName map[string]providerEntry
	byHost map[string]string // host key -> provider name
}

// emptyProviders is the set with no provider.
func emptyProviders() *providerSet {
	return &providerSet{byName: map[string]providerEntry{}, byHost: map[string]string{}}
}

// buildProviderSet snapshots st. A provider whose host and credential equal those it had in prev
// keeps its generation; any other gets a fresh one from next. A nil st is the empty set.
func buildProviderSet(st *Store, prev *providerSet, next func() uint64) *providerSet {
	set := emptyProviders()
	if st == nil {
		return set
	}
	for name, e := range st.List() {
		hk, err := canonicalHostKey(e.BaseURL)
		if err != nil {
			// Store validated every entry, so this cannot happen; an entry that cannot be matched
			// would be unusable anyway.
			continue
		}
		pe := providerEntry{entry: e, hostKey: hk}
		if old, ok := prev.byName[name]; ok && old.hostKey == hk && sameCredential(old.entry, e) {
			pe.gen = old.gen
		} else {
			pe.gen = next()
		}
		set.byName[name] = pe
		set.byHost[hk] = name
	}
	return set
}

// sameCredential reports whether a and b authenticate identically.
func sameCredential(a, b LocalEntry) bool {
	return a.Auth == b.Auth && a.Username == b.Username && a.Token.Reveal() == b.Token.Reveal()
}

// match returns the provider serving hostKey. An empty key (a repository with no valid origin)
// matches nothing.
func (s *providerSet) match(hostKey string) (providerEntry, string, bool) {
	if hostKey == "" {
		return providerEntry{}, "", false
	}
	name, ok := s.byHost[hostKey]
	if !ok {
		return providerEntry{}, "", false
	}
	return s.byName[name], name, true
}

// equal reports whether s and o are the same configuration: the same providers with identical
// entries. A change to an entry that does not touch its credential (its base URL's path) is a
// different configuration, because the origin probe dials that URL.
func (s *providerSet) equal(o *providerSet) bool {
	if len(s.byName) != len(o.byName) {
		return false
	}
	for name, pe := range s.byName {
		oe, ok := o.byName[name]
		if !ok || pe.hostKey != oe.hostKey || pe.entry.BaseURL != oe.entry.BaseURL || !sameCredential(pe.entry, oe.entry) {
			return false
		}
	}
	return true
}

// entries returns the providers' entries, ordered by name.
func (s *providerSet) entries() []LocalEntry {
	names := make([]string, 0, len(s.byName))
	for n := range s.byName {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]LocalEntry, len(names))
	for i, n := range names {
		out[i] = s.byName[n].entry
	}
	return out
}
