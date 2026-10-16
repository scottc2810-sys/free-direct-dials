# Free Direct Dials

A free B2B contact directory at `markemindai.com/free-direct-dials`. Visitors filter about a million contacts by country, industry, seniority, technology and more. After confirming a real email and a short sponsor break (or a Google rewarded video), each break unlocks **10 contacts they have never seen**, shown the moment the ad ends. Anyone can browse 5 pages of masked rows; pages 2–5 each open after a short sponsor break. Revenue comes from Adsterra (sponsor break, Smart Link placements, banner slots), Google Ad Manager rewarded video, labelled partner (affiliate) offers and donations (UPI, PayPal, crypto). Custom and bulk data requests go to WhatsApp (+91 98343 46179).

One Go binary + SQLite + Caddy, behind Cloudflare, sized for a **0.75 GB RAM / 20 GB disk** VPS. Contacts can also be spread over **Supabase / Postgres databases** added in Superadmin → Databases; the VPS database is searched first and the others only when needed.

| Doc | What's in it |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | system design, data model, filter contract, security, spec corrections |
| [docs/PLAN.md](docs/PLAN.md) | build plan, acceptance tests mapped to code |
| [docs/DEPLOY.md](docs/DEPLOY.md) | **step-by-step deployment on a VM with PuTTY** |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | Cloudflare + VPS setup, deploys, backups, alerts, admin IP allow-list |
| [docs/PERFORMANCE.md](docs/PERFORMANCE.md) | measured numbers at 1 M contacts and tuning notes |
| [docs/REVIEW.md](docs/REVIEW.md) | senior review findings and their resolution |
| [docs/PRODUCT.md](docs/PRODUCT.md) | product and design brief |
| [docs/MONETIZATION.md](docs/MONETIZATION.md) | which ad networks and affiliates, how to sign up, ads.txt, how to earn more |

## Run locally (Windows, macOS, Linux)

```bash
go build -o bin/dials ./cmd/dials
./bin/dials gen 20000 data/sample.csv && ./bin/dials import data/sample.csv
./bin/dials admin create yourname        # prints a TOTP QR for your authenticator
./bin/dials serve                        # http://127.0.0.1:8081/free-direct-dials/
```

In dev, sign-in codes print to the console (`DEV sign-in code`), Turnstile is off, and the superadmin panel at `/free-direct-dials/dev-admin-0123456789/` is reachable only from localhost.

## Test

```bash
go vet ./... && go test ./... -count=1
go test ./internal/filter/ -run XXX -fuzz FuzzCanonicalRoundTrip -fuzztime 60s
DIALS_SAMPLE_XLSX=/path/to/vendor-export.xlsx go test ./internal/importer/ -run TestRealSampleIfPresent -v
# Postgres store against a THROWAWAY Supabase project (drops schema fdd there):
# put DIALS_TEST_PG_DSN=postgresql://... in data/test-supabase.env, then
go test ./internal/store/ -run Postgres -v
```

## CLI

`serve` · `migrate` · `admin create <user>` · `import <file> [-sheet N] [-target auto|ID]` · `stores` · `inspect <file>` · `gen <rows> <out.csv>` · `backup` · `restore <src> <dst>` · `seo` · `blocklist <file>` · `bench [-plans]` · `check` · `version`

## Deploy

**On a VM with PuTTY (Windows):** follow [docs/DEPLOY.md](docs/DEPLOY.md). Download a ready bundle from [Releases](https://github.com/scottc2810-sys/free-direct-dials/releases), then on the VM:

```bash
ADMIN_IPS="<your.public.ip>" bash deploy/install.sh    # first install (from the extracted bundle)
bash deploy/upgrade.sh                                 # later releases: backup, switch, health check, auto-rollback
```

From a dev machine with Go and SSH instead:

```bash
ADMIN_IPS="203.0.113.7" bash deploy/provision.sh    # once, on the VPS, as root
deploy/deploy.sh admin@your-vps                      # every release: test, build, backup, swap, health-gate, auto-rollback
```

Build the bundles yourself with `bash deploy/make-release.sh v1.0.0` (writes `dist/`). Pushing a `v*` tag makes GitHub Actions build and publish them.
