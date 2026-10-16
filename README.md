# Free Direct Dials

A free B2B contact directory at `markemindai.com/free-direct-dials`. Visitors filter about a million contacts by country, industry, seniority, technology and more. After confirming a real email and a short sponsor break, each break unlocks **10 contacts they have never seen**. Revenue comes from Adsterra (sponsor break, Smart Link placements, banner slots) and donations (UPI, PayPal, crypto). Custom and bulk data requests go to WhatsApp.

One Go binary + SQLite + Caddy, behind Cloudflare, sized for a **0.75 GB RAM / 20 GB disk** VPS.

| Doc | What's in it |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | system design, data model, filter contract, security, spec corrections |
| [docs/PLAN.md](docs/PLAN.md) | build plan, acceptance tests mapped to code |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | Cloudflare + VPS setup, deploys, backups, alerts, admin IP allow-list |
| [docs/PERFORMANCE.md](docs/PERFORMANCE.md) | measured numbers at 1 M contacts and tuning notes |
| [docs/REVIEW.md](docs/REVIEW.md) | senior review findings and their resolution |
| [docs/PRODUCT.md](docs/PRODUCT.md) | product and design brief |

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
```

## CLI

`serve` · `migrate` · `admin create <user>` · `import <file> [-sheet N]` · `inspect <file>` · `gen <rows> <out.csv>` · `backup` · `restore <src> <dst>` · `seo` · `blocklist <file>` · `bench [-plans]` · `check` · `version`

## Deploy

```bash
ADMIN_IPS="203.0.113.7" bash deploy/provision.sh    # once, on the VPS, as root
deploy/deploy.sh admin@your-vps                      # every release: test, build, backup, swap, health-gate, auto-rollback
```
