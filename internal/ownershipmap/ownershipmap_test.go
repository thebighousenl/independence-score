package ownershipmap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseValid(t *testing.T) {
	data := []byte(`
- domain: graph.facebook.com
  company: Meta
  bucket: company
  category: social
- domain: facebook.com
  company: Meta
  bucket: company
  category: social
- domain: nextcloud.com
  company: Nextcloud
  bucket: self-hosted
  category: file-sync
`)
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Entries()) != 3 {
		t.Fatalf("got %d entries, want 3", len(m.Entries()))
	}
}

func TestParseRejectsDuplicateDomain(t *testing.T) {
	data := []byte(`
- domain: example.com
  company: Example
  bucket: company
- domain: example.com
  company: Example
  bucket: company
`)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected error for duplicate domain, got nil")
	}
}

func TestParseRejectsMissingOwner(t *testing.T) {
	data := []byte(`
- domain: example.com
  bucket: company
`)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected error for missing owner, got nil")
	}
}

func TestParseRejectsBadBucket(t *testing.T) {
	data := []byte(`
- domain: example.com
  company: Example
  bucket: nonsense
`)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected error for invalid bucket, got nil")
	}
}

func TestParseRejectsEmptyDomain(t *testing.T) {
	data := []byte(`
- domain: ""
  company: Example
  bucket: company
`)
	if _, err := Parse(data); err == nil {
		t.Fatal("expected error for empty domain, got nil")
	}
}

func TestLookupSpecificSubdomainWinsOverCatchAll(t *testing.T) {
	data := []byte(`
- domain: facebook.com
  company: Meta
  bucket: company
  category: social
- domain: graph.facebook.com
  company: Meta
  bucket: company
  category: api
`)
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	e, ok := m.Lookup("graph.facebook.com")
	if !ok {
		t.Fatal("expected match for graph.facebook.com")
	}
	if e.Category != "api" {
		t.Fatalf("expected specific entry (category=api) to win, got category=%s", e.Category)
	}

	e, ok = m.Lookup("www.facebook.com")
	if !ok {
		t.Fatal("expected match for www.facebook.com")
	}
	if e.Category != "social" {
		t.Fatalf("expected catch-all entry (category=social) to match, got category=%s", e.Category)
	}
}

func TestLookupNoMatch(t *testing.T) {
	data := []byte(`
- domain: facebook.com
  company: Meta
  bucket: company
`)
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := m.Lookup("example.org"); ok {
		t.Fatal("expected no match for example.org")
	}
}

func TestLookupDoesNotMatchStringSuffixAcrossLabels(t *testing.T) {
	// "notfacebook.com" must not match an entry for "facebook.com": the
	// match has to be on whole DNS labels, not a raw string suffix.
	data := []byte(`
- domain: facebook.com
  company: Meta
  bucket: company
`)
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := m.Lookup("notfacebook.com"); ok {
		t.Fatal("expected notfacebook.com not to match facebook.com entry")
	}
}

func TestLookupTrailingDotAndCase(t *testing.T) {
	data := []byte(`
- domain: Facebook.com
  company: Meta
  bucket: company
`)
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := m.Lookup("GRAPH.FACEBOOK.COM."); !ok {
		t.Fatal("expected case-insensitive, trailing-dot-tolerant match")
	}
}

func TestLoadRootOwnershipMap(t *testing.T) {
	// The repo-root ownership-map.yaml must itself parse and validate
	// cleanly; this is the CI schema check for that file.
	root := findRepoRoot(t)
	m, err := Load(filepath.Join(root, "ownership-map.yaml"))
	if err != nil {
		t.Fatalf("loading repo-root ownership-map.yaml: %v", err)
	}
	if len(m.Entries()) == 0 {
		t.Fatal("expected at least one entry in ownership-map.yaml")
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod) from " + dir)
		}
		dir = parent
	}
}
