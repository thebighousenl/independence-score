// Package classifier assigns each queried hostname to exactly one bucket:
// a known company, a known self-hosted category, or unclassified. It reads
// only the hostname, never a resolved IP.
package classifier

import "github.com/thebighousenl/independence-score/internal/ownershipmap"

// BucketType is one of the three classification outcomes.
type BucketType string

const (
	BucketCompany      BucketType = "company"
	BucketSelfHosted   BucketType = "self-hosted"
	BucketUnclassified BucketType = "unclassified"
)

// Result is the outcome of classifying one hostname.
type Result struct {
	Bucket BucketType
	// Owner is the company or self-hosted software name, empty when
	// Bucket is BucketUnclassified.
	Owner string
	// Category is the owning entry's category tag, empty when
	// Bucket is BucketUnclassified or the entry has no category.
	Category string
}

// Classifier classifies hostnames against a loaded ownership map.
type Classifier struct {
	m *ownershipmap.Map
}

// New builds a Classifier over an already-loaded, validated map.
func New(m *ownershipmap.Map) *Classifier {
	return &Classifier{m: m}
}

// Classify buckets a single hostname. It never inspects anything but the
// hostname string.
func (c *Classifier) Classify(hostname string) Result {
	entry, ok := c.m.Lookup(hostname)
	if !ok {
		return Result{Bucket: BucketUnclassified}
	}

	bucket := BucketCompany
	if entry.Bucket == ownershipmap.BucketSelfHosted {
		bucket = BucketSelfHosted
	}
	return Result{
		Bucket:   bucket,
		Owner:    entry.Owner,
		Category: entry.Category,
	}
}
