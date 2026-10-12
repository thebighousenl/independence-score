# Independence Score

**Status: pre-release. There is no working build yet. Do not point your router at this.**

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

## Design

One Docker container, written in Go, with a SQLite file. Four pieces: a DNS forwarder on
UDP/TCP 53, a store, a classifier, and a weekly diff job. No database server, no queue,
no external service.

## The ownership map

Classification is only as good as `ownership-map.yaml` — a flat domain-to-company list,
MIT licensed so it is reusable on its own, and grown by pull request. A domain that is
missing from it shows up as **unclassified**, with its own share of traffic, never folded
into either side. "We do not know" is always preferred over a confident wrong answer.

Contribution guide and schema land with the first release.

## Licence

MIT. See [LICENSE](LICENSE).
