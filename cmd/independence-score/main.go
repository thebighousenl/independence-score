// Command independence-score runs the DNS forwarder, store, classifier
// roll-up loop, and one-day dashboard as a single process.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thebighousenl/independence-score/internal/classifier"
	"github.com/thebighousenl/independence-score/internal/dashboard"
	"github.com/thebighousenl/independence-score/internal/forwarder"
	"github.com/thebighousenl/independence-score/internal/ownershipmap"
	"github.com/thebighousenl/independence-score/internal/store"
)

func main() {
	cfg := loadConfig()

	m, err := ownershipmap.Load(cfg.OwnershipMapPath)
	if err != nil {
		log.Fatalf("loading ownership map: %v", err)
	}
	log.Printf("loaded %d ownership map entries from %s", len(m.Entries()), cfg.OwnershipMapPath)

	st, err := store.Open(cfg.DBPath, cfg.RawRetention)
	if err != nil {
		log.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := st.SaveOwnershipMap(ctx, m.Entries()); err != nil {
		log.Fatalf("saving ownership map to store: %v", err)
	}

	cl := classifier.New(m)
	classify := func(hostname string) (store.BucketType, string) {
		r := cl.Classify(hostname)
		switch r.Bucket {
		case classifier.BucketCompany:
			return store.BucketCompany, r.Owner
		case classifier.BucketSelfHosted:
			return store.BucketSelfHosted, r.Owner
		default:
			return store.BucketUnclassified, ""
		}
	}

	srv := &forwarder.Server{
		Addr:     cfg.ListenAddr,
		Upstream: cfg.Upstream,
		Logger:   queryLogger{st},
	}
	go func() {
		log.Printf("forwarding DNS on %s to upstream %s", cfg.ListenAddr, cfg.Upstream)
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("forwarder stopped: %v", err)
		}
	}()

	go runRollUpLoop(ctx, st, classify, cfg.RollUpInterval)
	go runPruneLoop(ctx, st, cfg.RollUpInterval*10)
	go runDroppedQueryWatchLoop(ctx, st, cfg.RollUpInterval)

	dashHandler := &dashboard.Handler{Store: st, Classify: classify}
	go func() {
		log.Printf("dashboard listening on %s", cfg.DashboardAddr)
		if err := http.ListenAndServe(cfg.DashboardAddr, dashHandler); err != nil {
			log.Fatalf("dashboard stopped: %v", err)
		}
	}()

	waitForShutdown()
}

// queryLogger adapts store.Store's QueryEvent-based LogQuery to the
// forwarder.Logger interface's (hostname, qtype, at) signature.
type queryLogger struct{ s *store.Store }

func (q queryLogger) LogQuery(hostname string, qtype uint16, at time.Time) {
	q.s.LogQuery(store.QueryEvent{Hostname: hostname, Qtype: qtype, At: at})
}

func runRollUpLoop(ctx context.Context, st *store.Store, classify store.ClassifyFunc, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.RollUp(ctx, classify); err != nil {
				log.Printf("roll-up error: %v", err)
			}
		}
	}
}

func runPruneLoop(ctx context.Context, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.Prune(ctx, time.Now()); err != nil {
				log.Printf("prune error: %v", err)
			}
		}
	}
}

// runDroppedQueryWatchLoop polls Store.Stats on interval and logs when the
// dropped-query count has grown since the last check. A dropped query
// never fails or delays a lookup (LogQuery is fire-and-forget), so this is
// the only place that count is otherwise visible: without it, a store
// that is falling behind the query rate would do so silently.
func runDroppedQueryWatchLoop(ctx context.Context, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastDropped int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dropped := st.Stats()
			if dropped > lastDropped {
				log.Printf("store dropped %d queries since last check (total dropped: %d) — the store is falling behind the query rate; queries were still forwarded and answered, only their log record was lost", dropped-lastDropped, dropped)
			}
			lastDropped = dropped
		}
	}
}

func waitForShutdown() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
}
