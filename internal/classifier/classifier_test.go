package classifier

import (
	"testing"

	"github.com/thebighousenl/independence-score/internal/ownershipmap"
)

func mustMap(t *testing.T, yaml string) *ownershipmap.Map {
	t.Helper()
	m, err := ownershipmap.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

const fixture = `
- domain: facebook.com
  company: Meta
  bucket: company
  category: social
- domain: graph.facebook.com
  company: Meta
  bucket: company
  category: api
- domain: googleapis.com
  company: Google
  bucket: company
  category: cloud-infra
- domain: nextcloud.com
  company: Nextcloud
  bucket: self-hosted
  category: file-sync
`

func TestClassifyKnownCompany(t *testing.T) {
	c := New(mustMap(t, fixture))
	r := c.Classify("www.facebook.com")
	if r.Bucket != BucketCompany || r.Owner != "Meta" || r.Category != "social" {
		t.Fatalf("got %+v", r)
	}
}

func TestClassifySpecificSubdomainBeatsCatchAll(t *testing.T) {
	c := New(mustMap(t, fixture))
	r := c.Classify("graph.facebook.com")
	if r.Bucket != BucketCompany || r.Owner != "Meta" || r.Category != "api" {
		t.Fatalf("expected the specific graph.facebook.com entry to win, got %+v", r)
	}
}

func TestClassifyKnownSelfHosted(t *testing.T) {
	c := New(mustMap(t, fixture))
	r := c.Classify("updates.nextcloud.com")
	if r.Bucket != BucketSelfHosted || r.Owner != "Nextcloud" {
		t.Fatalf("got %+v", r)
	}
}

func TestClassifyUnclassified(t *testing.T) {
	c := New(mustMap(t, fixture))
	r := c.Classify("some-random-self-hosted-domain.example")
	if r.Bucket != BucketUnclassified {
		t.Fatalf("got %+v, want unclassified", r)
	}
	if r.Owner != "" || r.Category != "" {
		t.Fatalf("unclassified result must not carry an owner or category, got %+v", r)
	}
}

func TestClassifyNeverUsesSubstringMatch(t *testing.T) {
	c := New(mustMap(t, fixture))
	r := c.Classify("notgoogleapis.com")
	if r.Bucket != BucketUnclassified {
		t.Fatalf("expected unclassified for a non-label suffix, got %+v", r)
	}
}
