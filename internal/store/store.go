// Package store is the single SQLite file backing Independence Score: a
// short-retention log of raw queries, long-retention daily classified
// aggregates, and a copy of the loaded ownership map.
//
// LogQuery is the only entry point reachable from the DNS forwarder's hot
// path. It never blocks: it hands the event to a buffered channel drained
// by one background writer goroutine, and drops the event if that buffer
// is full rather than ever slow down or fail a lookup.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/publicsuffix"
	_ "modernc.org/sqlite"

	"github.com/thebighousenl/independence-score/internal/ownershipmap"
)

// QueryEvent is one observed DNS query, as recorded by the forwarder.
type QueryEvent struct {
	Hostname string
	Qtype    uint16
	At       time.Time
}

// BucketType mirrors classifier.BucketType without importing the
// classifier package, keeping the store a pure persistence layer.
type BucketType string

const (
	BucketCompany      BucketType = "company"
	BucketSelfHosted   BucketType = "self-hosted"
	BucketUnclassified BucketType = "unclassified"
)

// ClassifyFunc buckets a hostname. The store calls this during roll-up; it
// never classifies on the hot logging path.
type ClassifyFunc func(hostname string) (bucket BucketType, owner string)

// DayShare is one bucket/owner's query count for a single day.
type DayShare struct {
	Bucket BucketType
	Owner  string
	Count  int64
}

// Store owns the SQLite file.
type Store struct {
	db        *sql.DB
	queue     chan QueryEvent
	dropped   atomic.Int64
	done      chan struct{}
	stopped   chan struct{}
	retention time.Duration
}

const queueSize = 4096

// unclassifiedDomainsCap is how many registered domains RollUp keeps per
// day in unclassified_domains. It is the evidence budget for "why is this
// day's unclassified share high": enough to show whether a handful of
// domains dominate it, without growing the long-retention table
// unboundedly as a long tail of one-off hostnames is pruned in.
const unclassifiedDomainsCap = 25

// Open opens (creating if needed) the SQLite file at path and runs
// migrations. retention is how long raw query rows are kept; Prune removes
// rows older than that.
func Open(path string, retention time.Duration) (*Store, error) {
	// A single-writer, WAL-mode SQLite file matches the one-process,
	// one-file design: no server, no concurrent-writer problem to solve.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{
		db:        db,
		queue:     make(chan QueryEvent, queueSize),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
		retention: retention,
	}
	go s.writeLoop()
	return s, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS raw_queries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			hostname TEXT NOT NULL,
			qtype INTEGER NOT NULL,
			queried_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_raw_queries_queried_at ON raw_queries(queried_at)`,
		`CREATE TABLE IF NOT EXISTS daily_aggregates (
			day TEXT NOT NULL,
			bucket_type TEXT NOT NULL,
			owner TEXT NOT NULL,
			count INTEGER NOT NULL,
			PRIMARY KEY (day, bucket_type, owner)
		)`,
		`CREATE TABLE IF NOT EXISTS rollup_state (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO rollup_state (key, value) VALUES ('last_raw_id', 0)`,
		`CREATE TABLE IF NOT EXISTS ownership_map (
			domain TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			bucket TEXT NOT NULL,
			category TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS unclassified_domains (
			day TEXT NOT NULL,
			domain TEXT NOT NULL,
			count INTEGER NOT NULL,
			PRIMARY KEY (day, domain)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migrating store: %w", err)
		}
	}
	return nil
}

// LogQuery enqueues a query event for recording. It never blocks: if the
// internal buffer is full, the event is dropped and counted in Stats.
func (s *Store) LogQuery(ev QueryEvent) {
	select {
	case s.queue <- ev:
	default:
		s.dropped.Add(1)
	}
}

// Stats reports how many logged events were dropped because the internal
// buffer was full, since the store opened.
func (s *Store) Stats() (dropped int64) {
	return s.dropped.Load()
}

func (s *Store) writeLoop() {
	defer close(s.stopped)
	const flushEvery = 500 * time.Millisecond
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	batch := make([]QueryEvent, 0, 256)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertBatch(batch); err != nil {
			log.Printf("store: dropping batch of %d query rows after insert error: %v", len(batch), err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-s.queue:
			batch = append(batch, ev)
			if len(batch) >= cap(batch) {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			// Drain whatever is already queued, then exit.
			for {
				select {
				case ev := <-s.queue:
					batch = append(batch, ev)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Store) insertBatch(batch []QueryEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO raw_queries (hostname, qtype, queried_at) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, ev := range batch {
		if _, err := stmt.Exec(ev.Hostname, ev.Qtype, ev.At.UTC().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close stops the background writer, flushing anything queued, and closes
// the database.
func (s *Store) Close() error {
	close(s.done)
	<-s.stopped
	return s.db.Close()
}

// RollUp classifies every raw row recorded since the last call and folds
// the result into daily_aggregates. It is safe to call periodically and
// concurrently with LogQuery.
func (s *Store) RollUp(ctx context.Context, classify ClassifyFunc) error {
	var lastID int64
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM rollup_state WHERE key = 'last_raw_id'`).Scan(&lastID); err != nil {
		return fmt.Errorf("reading rollup cursor: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, hostname, queried_at FROM raw_queries WHERE id > ? ORDER BY id`, lastID)
	if err != nil {
		return fmt.Errorf("reading new raw queries: %w", err)
	}

	type key struct {
		day    string
		bucket BucketType
		owner  string
	}
	type domainKey struct {
		day    string
		domain string
	}
	deltas := make(map[key]int64)
	domainDeltas := make(map[domainKey]int64)
	touchedDays := make(map[string]struct{})
	maxID := lastID
	for rows.Next() {
		var id, queriedAt int64
		var hostname string
		if err := rows.Scan(&id, &hostname, &queriedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scanning raw query row: %w", err)
		}
		bucket, owner := classify(hostname)
		day := time.Unix(queriedAt, 0).UTC().Format("2006-01-02")
		deltas[key{day, bucket, owner}]++
		if bucket == BucketUnclassified {
			domainDeltas[domainKey{day, registeredDomain(hostname)}]++
			touchedDays[day] = struct{}{}
		}
		if id > maxID {
			maxID = id
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterating raw query rows: %w", err)
	}
	rows.Close()

	if maxID == lastID {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO daily_aggregates (day, bucket_type, owner, count)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (day, bucket_type, owner) DO UPDATE SET count = count + excluded.count
	`)
	if err != nil {
		return err
	}
	defer upsert.Close()

	for k, delta := range deltas {
		if _, err := upsert.ExecContext(ctx, k.day, string(k.bucket), k.owner, delta); err != nil {
			return fmt.Errorf("upserting daily aggregate: %w", err)
		}
	}

	domainUpsert, err := tx.PrepareContext(ctx, `
		INSERT INTO unclassified_domains (day, domain, count)
		VALUES (?, ?, ?)
		ON CONFLICT (day, domain) DO UPDATE SET count = count + excluded.count
	`)
	if err != nil {
		return err
	}
	defer domainUpsert.Close()

	for k, delta := range domainDeltas {
		if _, err := domainUpsert.ExecContext(ctx, k.day, k.domain, delta); err != nil {
			return fmt.Errorf("upserting unclassified domain: %w", err)
		}
	}

	// Trim each touched day back down to its top unclassifiedDomainsCap
	// domains by count. This runs every roll-up rather than once at the
	// end, so the table never grows past its cap even under sustained
	// traffic from a long tail of one-off hostnames.
	trim, err := tx.PrepareContext(ctx, `
		DELETE FROM unclassified_domains
		WHERE day = ? AND domain NOT IN (
			SELECT domain FROM unclassified_domains WHERE day = ? ORDER BY count DESC LIMIT ?
		)
	`)
	if err != nil {
		return err
	}
	defer trim.Close()

	for day := range touchedDays {
		if _, err := trim.ExecContext(ctx, day, day, unclassifiedDomainsCap); err != nil {
			return fmt.Errorf("trimming unclassified domains for %s: %w", day, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE rollup_state SET value = ? WHERE key = 'last_raw_id'`, maxID); err != nil {
		return fmt.Errorf("advancing rollup cursor: %w", err)
	}

	return tx.Commit()
}

// Prune deletes raw query rows older than the store's retention window.
// Daily aggregates, which hold the long-retention history, are untouched.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-s.retention).UTC().Unix()
	_, err := s.db.ExecContext(ctx, `DELETE FROM raw_queries WHERE queried_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("pruning raw queries: %w", err)
	}
	return nil
}

// DayShares returns the classified counts for the given UTC day
// (YYYY-MM-DD), one row per bucket/owner.
func (s *Store) DayShares(ctx context.Context, day string) ([]DayShare, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT bucket_type, owner, count FROM daily_aggregates
		WHERE day = ?
		ORDER BY count DESC
	`, day)
	if err != nil {
		return nil, fmt.Errorf("reading day shares: %w", err)
	}
	defer rows.Close()

	var out []DayShare
	for rows.Next() {
		var ds DayShare
		var bucket string
		if err := rows.Scan(&bucket, &ds.Owner, &ds.Count); err != nil {
			return nil, fmt.Errorf("scanning day share: %w", err)
		}
		ds.Bucket = BucketType(bucket)
		out = append(out, ds)
	}
	return out, rows.Err()
}

// TrendRow is one bucket/owner's query count for one day of the trend
// window.
type TrendRow struct {
	Day    string
	Bucket BucketType
	Owner  string
	Count  int64
}

// Trend returns the classified counts for every UTC day in
// [startDay, endDay] (both YYYY-MM-DD, inclusive) that has at least one
// daily aggregate row. A day with no rows - because it predates the
// instance's history, or simply had no traffic - is absent from the
// result rather than returned as a zero row; callers must not assume
// every calendar day in the range appears. Aggregates are long-retention
// by design (see Prune), so a day's trend data outlives its raw rows.
func (s *Store) Trend(ctx context.Context, startDay, endDay string) ([]TrendRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT day, bucket_type, owner, count FROM daily_aggregates
		WHERE day BETWEEN ? AND ?
		ORDER BY day ASC, count DESC
	`, startDay, endDay)
	if err != nil {
		return nil, fmt.Errorf("reading trend: %w", err)
	}
	defer rows.Close()

	var out []TrendRow
	for rows.Next() {
		var tr TrendRow
		var bucket string
		if err := rows.Scan(&tr.Day, &bucket, &tr.Owner, &tr.Count); err != nil {
			return nil, fmt.Errorf("scanning trend row: %w", err)
		}
		tr.Bucket = BucketType(bucket)
		out = append(out, tr)
	}
	return out, rows.Err()
}

// SaveOwnershipMap replaces the stored copy of the ownership map with the
// entries currently loaded from ownership-map.yaml. It runs once at
// startup, off the query path.
func (s *Store) SaveOwnershipMap(ctx context.Context, entries []ownershipmap.Entry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM ownership_map`); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO ownership_map (domain, owner, bucket, category) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.ExecContext(ctx, e.Domain, e.Owner, string(e.Bucket), e.Category); err != nil {
			return fmt.Errorf("saving ownership map entry %s: %w", e.Domain, err)
		}
	}

	return tx.Commit()
}

// DomainCount is one registered domain's query count for a day.
type DomainCount struct {
	Domain string
	Count  int64
}

// registeredDomain collapses a queried hostname down to its registered
// domain (eTLD+1) so a view grouping unclassified traffic shows "the
// operator resolved N registered domains it doesn't know", not one row
// per distinct subdomain. publicsuffix is used instead of a naive
// last-two-labels rule because multi-label public suffixes (co.uk,
// github.io, ...) would otherwise group unrelated domains together. A
// hostname publicsuffix cannot resolve a registered domain for - a bare
// public suffix, a single-label name, an IP literal - falls back to the
// lowercased hostname itself so grouping never errors out of RollUp.
func registeredDomain(hostname string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	if h == "" {
		return h
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return h
	}
	return d
}

// topDomainCounts sorts counts by count descending (domain name
// ascending to break ties deterministically) and truncates to limit.
func topDomainCounts(counts map[string]int64, limit int) []DomainCount {
	out := make([]DomainCount, 0, len(counts))
	for d, c := range counts {
		out = append(out, DomainCount{Domain: d, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Domain < out[j].Domain
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// RawRetentionCutoff returns the oldest instant still covered by
// raw_queries retention, as of now. A day whose entire span ends at or
// before this instant has had every one of its raw rows pruned, so
// UnclassifiedDomainsLive can no longer see any of it.
func (s *Store) RawRetentionCutoff(now time.Time) time.Time {
	return now.Add(-s.retention)
}

// UnclassifiedDomainsLive classifies every raw row still held for day and
// groups the unclassified ones by registered domain, highest query count
// first - evidence for whether a day's unclassified share is a few
// missing companies or a long tail of ordinary sites. It only ever reads
// rows still in raw_queries: a day already pruned past the retention
// window returns an empty slice with no error. Callers must check
// RawRetentionCutoff themselves before treating an empty result as
// "nothing was unclassified" rather than "too old to still have raw
// rows".
func (s *Store) UnclassifiedDomainsLive(ctx context.Context, day string, classify ClassifyFunc) ([]DomainCount, error) {
	start, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil, fmt.Errorf("parsing day %q: %w", day, err)
	}
	startUnix := start.UTC().Unix()
	endUnix := start.UTC().AddDate(0, 0, 1).Unix()

	rows, err := s.db.QueryContext(ctx, `SELECT hostname FROM raw_queries WHERE queried_at >= ? AND queried_at < ?`, startUnix, endUnix)
	if err != nil {
		return nil, fmt.Errorf("reading raw queries for %s: %w", day, err)
	}
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var hostname string
		if err := rows.Scan(&hostname); err != nil {
			return nil, fmt.Errorf("scanning raw query hostname: %w", err)
		}
		bucket, _ := classify(hostname)
		if bucket != BucketUnclassified {
			continue
		}
		counts[registeredDomain(hostname)]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating raw query rows: %w", err)
	}

	return topDomainCounts(counts, unclassifiedDomainsCap), nil
}

// UnclassifiedDomainsHistory reads the capped per-day snapshot RollUp
// wrote to unclassified_domains for day. Unlike raw_queries, this table
// is never pruned, so it is the only source of per-domain unclassified
// evidence once a day falls outside the raw retention window - at the
// cost of being capped at unclassifiedDomainsCap domains rather than
// exhaustive.
func (s *Store) UnclassifiedDomainsHistory(ctx context.Context, day string) ([]DomainCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain, count FROM unclassified_domains WHERE day = ? ORDER BY count DESC, domain ASC`, day)
	if err != nil {
		return nil, fmt.Errorf("reading unclassified domain history for %s: %w", day, err)
	}
	defer rows.Close()

	var out []DomainCount
	for rows.Next() {
		var dc DomainCount
		if err := rows.Scan(&dc.Domain, &dc.Count); err != nil {
			return nil, fmt.Errorf("scanning unclassified domain row: %w", err)
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}
