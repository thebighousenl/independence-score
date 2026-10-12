# Independence Score

**Status: M1 ("I get a number"). The forwarder, store, classifier and a
one-day dashboard work and are covered by tests, but this has not yet run
as primary DNS on a real home network for 24 hours. Keep a secondary DNS
entry on your router before you try it (see below), and do not rely on it
yet as your only resolver.**

You run your own network, and you already block ads. This tool answers the question
blocking does not: *of the DNS traffic that gets through, how much still lands in
Google, Meta, Amazon, Microsoft and Apple — versus independent and self-hosted
services?*

It is its own DNS forwarder. You point your router's DNS at it, it passes every query
upstream unchanged (to a public resolver, or to the Pi-hole you already run), and it
classifies each hostname by who owns the **service**, not who hosts it. Nothing else has
to be installed, and nothing you already run has to be replaced.

You open it again next week not to relive last week's number, but because it tells you
what changed: a new company showing up in your traffic, a company's share crossing a
threshold, a domain that was "unclassified" last week and is not anymore.

## What it is not

- **Not a blocker.** It is forward-only: no blocklists, no answer rewriting, no NXDOMAIN
  injection. Anything it does not understand is forwarded anyway.
- **Not a resilient resolver.** It is one container. Always keep a secondary DNS entry on
  your router.
- **Not a tracker analyser**, and not multi-user.

## Running it

```
docker build -t independence-score .
docker run -d --name independence-score \
  -p 53:53/udp -p 53:53/tcp -p 8080:8080 \
  -v independence-score-data:/data \
  -e UPSTREAM_DNS=1.1.1.1:53 \
  independence-score
```

Then point your router's **primary** DNS entry at this container's address, and
set a **secondary** DNS entry to a resolver you already trust (your ISP's,
1.1.1.1, or the Pi-hole you already run). The secondary entry is required, not
optional: this is one container, not a resilient resolver, and your network
should keep working if it is ever down or being restarted.

Open `http://<container-host>:8080/` for the one-day dashboard: shares by
company, by self-hosted category, and the unclassified share, which is always
shown and never folded into either bucket.

Configuration is by environment variable:

| Variable              | Default                                  | Meaning                                  |
| --------------------- | ----------------------------------------- | ----------------------------------------- |
| `LISTEN_ADDR`         | `:53`                                      | Address the forwarder listens on          |
| `UPSTREAM_DNS`        | `1.1.1.1:53`                                | Resolver every query is forwarded to      |
| `DB_PATH`             | `/data/independence-score.sqlite`           | SQLite file (put this on the volume)      |
| `OWNERSHIP_MAP_PATH`  | `/etc/independence-score/ownership-map.yaml`| Path to the loaded ownership map          |
| `DASHBOARD_ADDR`      | `:8080`                                     | Address the dashboard HTTP server binds   |
| `RAW_RETENTION`       | `48h`                                        | How long raw query rows are kept          |
| `ROLLUP_INTERVAL`     | `10s`                                        | How often new rows are classified         |

### What this cannot see

Any device that speaks DNS-over-HTTPS or DNS-over-TLS directly to a public
resolver (many phone and browser defaults do this) bypasses this tool
entirely, and no local tool can measure it. The dashboard reports what it
actually observed, not a claim to see your whole network's traffic.

## Design

One Docker container, written in Go, with a SQLite file. Four pieces: a DNS forwarder on
UDP/TCP 53, a store, a classifier, and a weekly diff job (not yet built - see
Milestones). No database server, no queue, no external service.

## Milestones

- **M1 ("I get a number") - done.** Forwarder, store, classifier, seed
  ownership map, one-day dashboard.
- **M2 ("I trust it, and it remembers") - not started.** Full query-type
  conformance suite in CI, documented secondary-DNS failover, multi-day trend
  view. This, not M1, is the bar for a public release.
- **M3 ("It tells me when something changed") - not started.** Weekly diff
  job with webhook/email digests.

## The ownership map

Classification is only as good as [`ownership-map.yaml`](ownership-map.yaml) — a flat
domain-to-company list, MIT licensed so it is reusable on its own, and grown by pull
request. A domain that is missing from it shows up as **unclassified**, with its own
share of traffic, never folded into either side. "We do not know" is always preferred
over a confident wrong answer.

To add an entry: edit `ownership-map.yaml`, add one list entry (see the schema comment
at the top of the file), and open a pull request. The current seed list covers GAFAM and
some common self-hosted software; it is intentionally a starting point, not a finished
list.

## Licence

MIT. See [LICENSE](LICENSE).
