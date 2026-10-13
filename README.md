# Independence Score

**Status: M1 ("I get a number") is done** — a real user ran this as primary
DNS on a real home network for 24 hours with nothing breaking, and the
dashboard showed non-zero classified shares. **M2 ("I trust it, and it
remembers") is done**: the query-type conformance suite, documented
secondary-DNS failover, and the multi-day trend view below. Per the project
brief, M2 — not M1 — is the bar for a public release. Keep a secondary DNS
entry on your router regardless (see Failover below); this is one container,
not a resilient resolver.

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

Open `http://<container-host>:8080/` for the dashboard: today's shares by
company, by self-hosted category, and the unclassified share (always shown,
never folded into either bucket), plus a multi-day trend over the stored
daily aggregates beneath it. The trend defaults to a 14-day window
(`?trend_days=N` to change it) and states plainly how many of the requested
days actually have data — a new instance with one day of history renders as
one row, not a chart implying a longer trend than it has.

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

### Troubleshooting

**`docker run` fails with `address already in use` on port 53.** Something on the
host is already holding port 53. This is the most common first-run failure, not a
build problem.

Detect it:

```
sudo ss -lunp 'sport = :53'
```

If that prints a process, something is already bound to port 53 on the host.

- **`systemd-resolved`** (the default on Ubuntu and most Debian derivatives) runs a
  stub resolver on `127.0.0.1:53` by default. Fix:

  ```
  sudo sed -i 's/^#\?DNSStubListener=.*/DNSStubListener=no/' /etc/systemd/resolved.conf
  sudo systemctl restart systemd-resolved
  ```

  `/etc/resolv.conf` is usually a symlink to `/run/systemd/resolve/stub-resolv.conf`
  managed by `systemd-resolved`; after disabling the stub listener, make sure it (or
  whatever `/etc/resolv.conf` points to) still has a real `nameserver` line — for
  example `nameserver 1.1.1.1` — so the host itself can still resolve names while
  this container is starting.

  To undo: set `DNSStubListener=yes` back in `/etc/systemd/resolved.conf` (or remove
  the line to restore the default) and `sudo systemctl restart systemd-resolved`.

- **`dnsmasq`** or a locally-installed **Pi-hole** on the same host also bind port
  53. If you are already running either of those on this machine, you don't want
  this container competing with them for the port anyway — either run Pi-hole in
  its own container and point this tool's `UPSTREAM_DNS` at it, or run this tool on
  a different host. Stopping the conflicting service (`sudo systemctl stop dnsmasq`
  or stopping the Pi-hole container) only relocates the same problem; picking which
  one should actually own port 53 on this host is the real fix.

Both `golang:1.26-alpine` and `alpine:3.20` (the Dockerfile's base images) are
published on Docker Hub and `go.sum` is committed, so a missing base image or an
untracked `go.sum` are not expected causes of a `docker build` failure. A port 53
conflict on the host is the one most new users will actually hit.

### Failover

This is one container, not a resilient resolver, so your router's **secondary**
DNS entry is how the network survives this container being unavailable. Failover
only works because a stub resolver's normal trigger for trying the secondary
entry - getting no answer at all from the primary - actually fires. The
forwarder is built so that every unavailability case it can be in produces
exactly that: silence, never a fabricated answer.

There are two situations that look similar from a router's point of view but
are different problems to fix:

- **The container itself is down** (stopped, crashed, restarting). Nothing is
  listening on port 53 at all. A UDP query gets no response until the client's
  own timeout fires; a TCP connection attempt is refused immediately. Either
  way, the client moves on to the secondary entry.
- **The container is up, but its configured upstream (`UPSTREAM_DNS`) is
  unreachable** - the upstream is down, refusing connections, or not
  responding. The forwarder's listeners are still open and still accepting
  queries, but every query it tries to forward fails. It does **not**
  synthesise a SERVFAIL, an empty NOERROR, or any other answer to paper over
  that failure - a client that got any valid DNS message would treat it as
  authoritative and would not retry on the secondary entry, which is worse
  than the container being down outright. So this case produces the same
  silence a down container does, even though the process is alive.

To tell the two apart, check the container itself (`docker ps`, `docker logs`)
rather than inferring it from the client's behaviour, which looks identical in
both cases. If the container is running and its logs show repeated `upstream
... forward failed` lines, the second case is what's happening, and the fix is
to check `UPSTREAM_DNS` reachability from the container's network - not to
restart the container.

**The honest limit:** failover only works for a device that actually honours
its secondary DNS entry. A device that resolves once, caches this container's
address, and does not fall back to the secondary when that address stops
answering will lose resolution until it retries DNS on its own. This tool does
not and cannot fix that; it is a property of the client device, not something
a DNS forwarder can work around.

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
- **M2 ("I trust it, and it remembers") - done.** Full query-type
  conformance suite in CI (see `internal/forwarder`'s conformance and
  failover tests), documented secondary-DNS failover, multi-day trend
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
