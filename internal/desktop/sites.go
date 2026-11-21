package desktop

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// siteFile is the settings file: for each site, whether swampcy acts there as a
// person would. EverySite's entry is for sites with none of their own.
type siteFile struct {
	AsHuman map[string]bool `json:"as_human"`
}

// Sites keeps, by site, whether swampcy acts as a person would there, in a JSON
// file that every swampcy of the user shares: it is read again whenever it has
// changed, and written whole. It is one switch a site: everything a person's
// hand does differently (the pointer's path, where clicks land, the pace of
// typing and its slips, the wheel, sliders) goes on and off together.
type Sites struct {
	Path string

	mu    sync.Mutex
	stamp time.Time
	size  int64
	data  siteFile
	err   error
}

// EverySite names the setting for sites with none of their own.
const EverySite = "*"

// load reads the file when it is not the one last read. A missing file is no settings.
func (s *Sites) load() (siteFile, error) {
	fi, err := os.Stat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		s.data, s.err, s.stamp, s.size = siteFile{}, nil, time.Time{}, 0
		return s.data, nil
	}
	if err != nil {
		return siteFile{}, err
	}
	if fi.ModTime().Equal(s.stamp) && fi.Size() == s.size {
		return s.data, s.err
	}
	s.stamp, s.size, s.err = fi.ModTime(), fi.Size(), nil
	var data siteFile
	b, err := os.ReadFile(s.Path)
	if err == nil {
		err = json.Unmarshal(b, &data)
	}
	if err != nil {
		// What was last read stays in force: a file caught half edited must not
		// turn every site's setting off.
		s.err = fmt.Errorf("site settings %s: %w", s.Path, err)
		return s.data, s.err
	}
	s.data = data
	return s.data, nil
}

// For is whether to act as a person on host ("www.example.com", or with a port
// "localhost:3000"), by the most specific site that covers it, or else by
// EverySite. rule names the entry that decided, and is "" when none did.
func (s *Sites) For(host string) (asHuman bool, rule string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load() // with an error, what was last read
	for site := range data.AsHuman {
		if siteCovers(site, host) && len(site) > len(rule) {
			rule = site
		}
	}
	if rule == "" {
		if _, ok := data.AsHuman[EverySite]; ok {
			rule = EverySite
		}
	}
	return data.AsHuman[rule], rule, err
}

// siteCovers reports whether a setting for site holds on host: the same host, or
// a subdomain of it. A site with a port covers only that port; one without, all.
func siteCovers(site, host string) bool {
	if host == "" || site == "" || site == EverySite {
		return false
	}
	if !strings.Contains(site, ":") {
		host, _, _ = strings.Cut(host, ":")
	}
	return host == site || strings.HasSuffix(host, "."+site)
}

// All is every setting in the file, and the sites in order, EverySite first.
func (s *Sites) All() (sites []string, asHuman map[string]bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load() // with an error, what was last read
	for site := range data.AsHuman {
		sites = append(sites, site)
	}
	slices.SortFunc(sites, func(a, b string) int {
		return cmp.Or(cmp.Compare(boolInt(b == EverySite), boolInt(a == EverySite)), cmp.Compare(a, b))
	})
	return sites, data.AsHuman, err
}

// Set says whether to act as a person on a site (or on EverySite), or with nil
// takes the site's setting away.
func (s *Sites) Set(site string, asHuman *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return err // not written over: it may be a hand's unfinished edit
	}
	next := siteFile{AsHuman: map[string]bool{}}
	for k, v := range data.AsHuman {
		next.AsHuman[k] = v
	}
	if asHuman == nil {
		delete(next.AsHuman, site)
	} else {
		next.AsHuman[site] = *asHuman
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	// Written beside and renamed, so no reader sees half a file.
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".sites-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return err
	}
	s.stamp = time.Time{} // read it back
	return nil
}

// SiteName reads what an agent calls a site ("Amazon.com", "https://www.amazon.com/dp/1",
// "localhost:3000", "*") into the name its settings are kept under: the host in
// lower case without "www.", with its port when one is given.
func SiteName(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == EverySite {
		return s, nil
	}
	if !strings.Contains(s, "://") {
		s = "x://" + s
	}
	u, err := url.Parse(s)
	if err != nil || strings.TrimPrefix(u.Hostname(), "www.") == "" || strings.ContainsAny(u.Hostname(), "*? :") {
		return "", fmt.Errorf("%q is not a site: give a host like example.com or localhost:3000, or * for every site", s)
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	if u.Port() != "" {
		host += ":" + u.Port()
	}
	return host, nil
}

// siteOfURL is the site a page's address is on, as For takes it: its host with
// the port when the address names one, or "" for an address with no host.
func siteOfURL(addr string) string {
	u, err := url.Parse(strings.TrimSpace(addr))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if u.Port() != "" {
		host += ":" + u.Port()
	}
	return host
}
