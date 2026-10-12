// Package ownershipmap loads and validates the domain-to-owner map that
// drives classification. The map itself is human-editable YAML, not Go code.
package ownershipmap

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bucket identifies which of the three classification buckets an entry
// belongs to.
type Bucket string

const (
	BucketCompany    Bucket = "company"
	BucketSelfHosted Bucket = "self-hosted"
)

// Entry is one row of ownership-map.yaml. The on-disk field is named
// "company" to match the brief's locked schema; Owner (the Go field name)
// covers both a GAFAM-style company and a self-hosted project's own name.
type Entry struct {
	Domain   string `yaml:"domain"`
	Owner    string `yaml:"company"`
	Bucket   Bucket `yaml:"bucket"`
	Category string `yaml:"category"`
}

// Map is a validated, loaded ownership map ready for classification.
type Map struct {
	entries []Entry
}

// Load reads and validates an ownership map from path.
func Load(path string) (*Map, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ownership map %s: %w", path, err)
	}
	return Parse(data)
}

// Parse validates and builds a Map from raw YAML bytes.
func Parse(data []byte) (*Map, error) {
	var raw []Entry
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing ownership map: %w", err)
	}

	seen := make(map[string]struct{}, len(raw))
	entries := make([]Entry, 0, len(raw))
	for i, e := range raw {
		domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(e.Domain), "."))
		if domain == "" {
			return nil, fmt.Errorf("entry %d: domain is required", i)
		}
		if e.Owner == "" {
			return nil, fmt.Errorf("entry %d (%s): company is required", i, domain)
		}
		if e.Bucket != BucketCompany && e.Bucket != BucketSelfHosted {
			return nil, fmt.Errorf("entry %d (%s): bucket must be %q or %q, got %q", i, domain, BucketCompany, BucketSelfHosted, e.Bucket)
		}
		if _, dup := seen[domain]; dup {
			return nil, fmt.Errorf("entry %d: duplicate domain %q", i, domain)
		}
		seen[domain] = struct{}{}

		entries = append(entries, Entry{
			Domain:   domain,
			Owner:    e.Owner,
			Bucket:   e.Bucket,
			Category: e.Category,
		})
	}

	return &Map{entries: entries}, nil
}

// Entries returns the validated entries, in file order.
func (m *Map) Entries() []Entry {
	return m.entries
}

// Lookup returns the most specific entry whose domain is a suffix match of
// hostname (matched on whole DNS labels, not raw string suffix), and true if
// a match was found. A pinned subdomain entry always wins over a shorter
// catch-all entry for the same hostname, because it has more matching
// labels.
func (m *Map) Lookup(hostname string) (Entry, bool) {
	hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if hostname == "" {
		return Entry{}, false
	}
	hostLabels := strings.Split(hostname, ".")

	var best Entry
	bestLabels := -1
	found := false
	for _, e := range m.entries {
		entryLabels := strings.Split(e.Domain, ".")
		if len(entryLabels) > len(hostLabels) {
			continue
		}
		if !labelSuffixMatch(hostLabels, entryLabels) {
			continue
		}
		if len(entryLabels) > bestLabels {
			best = e
			bestLabels = len(entryLabels)
			found = true
		}
	}
	return best, found
}

// labelSuffixMatch reports whether entryLabels is exactly the trailing
// sequence of hostLabels, label-for-label.
func labelSuffixMatch(hostLabels, entryLabels []string) bool {
	offset := len(hostLabels) - len(entryLabels)
	for i, l := range entryLabels {
		if hostLabels[offset+i] != l {
			return false
		}
	}
	return true
}
