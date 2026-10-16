# Free Direct Dials — Implementation Plan

> Workflow: **Architect → Engineer → Reviewer → Optimizer** (Superpowers: brainstorm → plan → TDD execution → review → verification).
> Design: [ARCHITECTURE.md](ARCHITECTURE.md). Every task follows red → green → refactor; a task is done only when its tests pass under `go test ./...`.

## Team roles

| Role | Owns | Exit criteria |
|---|---|---|
| **Architect** | ARCHITECTURE.md, data model, API contract, spec corrections | Design covers every acceptance test; no open contradictions |
| **Engineer** | Phases 1–8 below | All unit + acceptance tests green; `go vet` clean; binary builds for linux/amd64 |
| **Reviewer** | Phase 9: code-review-graph impact map + senior review (correctness, security, edge cases) | Every finding fixed or explicitly accepted, with a reason |
| **Optimizer** | Phase 10: 1 M-row benchmark, query plans, memory under `GOMEMLIMIT`, page weight | p95 targets met (below); RSS within budget |

## Performance targets (1 M contacts, 1 vCPU, cold edge cache)

| Operation | p95 target |
|---|---|
| `GET /api/preview` (count + 10 masked), cache miss | ≤ 250 ms |
| `GET /api/facets` (one dimension), cache miss | ≤ 300 ms |
| `GET /api/facets` with no other filter (fast path) | ≤ 5 ms |
| `POST /api/ad/claim` | ≤ 50 ms |
| Import throughput | ≥ 5,000 rows/s |
| Process RSS under load | ≤ 300 MB |
| First view transfer (gzip) | ≤ 60 KB |

---

## Status (2026-10-15)

All phases 0–10 are **built and tested**. Results: [REVIEW.md](REVIEW.md) (findings) and [PERFORMANCE.md](PERFORMANCE.md) (measured numbers).
Added during the build at the founder's request: Smart Link placements everywhere (labelled), Excel `.xlsx` uploads with per-column mapping, dry run and remembered mappings, a **mandatory 3-layer superadmin IP allow-list**, DB hardening (encrypted backups, secure delete, owner-only files) and storage optimisation driven by a real Slintel export.

## Phase 0 — Tooling & scaffold
- [x] Install Go 1.27 (winget), Impeccable plugin, code-review-graph CLI.
- [ ] `go mod init markemind/dials`; layout per ARCHITECTURE §3; `.gitignore`, `.editorconfig`, `Makefile` + `build.ps1`.
- [ ] `git init` (no commits until the founder asks); register code-review-graph for the project.
- [ ] CI workflow `.github/workflows/ci.yml`: vet, `go test -race`, govulncheck, linux build artifact.

## Phase 1 — Data core
1. `platform`: injectable `Clock`, random ids/tokens, slog setup, disk stats (unix + windows).
2. `config`: env loader + validation (`APP_ENV=prod` refuses empty secrets or a short `ADMIN_PATH`).
3. `db`: open writer/readers with pragmas; embedded migrations (`schema_version`); tx helper; `meta` kv.
4. `normalize`: `Fold`, `IsBlank`, country aliases, `PhoneDigits` (≥6), `LinkedInKey`, `HashKey`.
   *Tests:* table-driven + fuzz (idempotence: `Fold(Fold(x)) == Fold(x)`).
5. `emailcheck.Syntax` + disposable matcher (subdomains + patterns). *Tests:* acceptance #8 (syntax part).
6. `importer`: sniff encoding (UTF-8 BOM, UTF-16 LE/BE, cp1252 fallback) and delimiter (`, ; \t |`); header alias map → canonical fields; row cleaning; batch upsert (dims dictionary, `dim_spell`, contacts, contact_info, tech, fts); suppression skip; skip-reason counters; progress callback; post-import label/count refresh + `dataset_version++` + `PRAGMA optimize`.
   *Tests:* acceptance #1 (USA/usa/" USA "), #3 (blank), #10 (re-upload → 0 new); encodings, ragged rows, quotes with embedded newlines, duplicate headers, formula-looking cells.
7. `cmd/dials gen <rows>`: deterministic synthetic CSV generator (used by tests + benchmarks).
8. `cmd/dials import <file>`: CLI import (for files copied over with scp).

## Phase 2 — Query engine
1. `filter`: parse from `url.Values`, canonical encode, limits, value folding; FTS sanitizer; tech terms. *Tests:* round-trip fuzz; acceptance #4 (`"; DROP --`, `%`, `_`, emoji → no error).
2. `filter.SQL`: WHERE builder over the narrow table, optionally excluding one dimension; value → dim-id resolution (unknown → matches nothing).
3. `search.Count`, `search.Preview` (masked), `search.Facets` (exclude own dim, top 300 + selected, `q`, `dims.cnt` fast path).
   *Tests:* acceptance #2 (OR within / AND across), #5 (**brute-force oracle**: random dataset + random filters, facet counts compared with a naive in-Go evaluation).
4. Byte-bounded LRU + singleflight + reader semaphore → `503 Retry-After`; 3 s query timeout.

## Phase 3 — Identity
1. `emailcheck`: MX/null-MX/implicit-A with 3 s timeout (fail-open), verdict + mx cache, optional business-email rule, Reacher client (20 s, concurrency 3, fail-open).
2. `auth.OTP`: HMAC hash, 10 min, 5 tries → lock, 3 sends/h/email, 10/h/IP, constant-time compare.
3. Sessions: random 32 B, stored as SHA-256, 30 d sliding (refresh at most once per day), `Path=/free-direct-dials`.
4. Turnstile verification (hostname + action check; disabled with a WARN in dev only).
5. `mail`: providers (brevo, resend, smtp, log), failover, quota with OTP reserve, OTP send path.
   *Tests:* acceptance #8 (mailinator, `sub.guerrillamail.com`, bad syntax rejected; 6th wrong OTP locked), with a fake provider.

## Phase 4 — Monetisation
1. `settings` store: typed sections (ads, donations, whatsapp, limits, email, site) with defaults + validation + audit.
2. `payments`: UPI VPA, PayPal URL, EVM EIP-55 (spec test vectors), BTC/SOL/TRON patterns; QR (rsc.io/qr) → compact SVG path.
3. `ads`: token start (mode, `min_seconds`), claim validation (age ≥ min−1, unused, same user, daily caps, min interval).
4. `search.Claim`: per-user mutex + writer tx → next 10 never-unlocked matching contacts, `batch = max+1`; `me/view` (batches, newest flag, `remaining`), CSV export (formula-safe).
   *Tests:* acceptance #6 (2 concurrent claims → 20 distinct), #7 (filter change → no re-issue), #9 (token < 9 s, reused, foreign → rejected).

## Phase 5 — HTTP layer & admin backend
1. Middleware: real-IP (from Caddy), request id, recover, security headers/CSP, cache policy, body limits, rate limits, CSRF/Origin checks.
2. Public + me + ad + auth handlers (contract in ARCHITECTURE §6 / §10). *Test:* public endpoints never `Set-Cookie`; personal endpoints are `no-store`.
3. Admin: argon2id + TOTP login, lockout, sessions, CSRF, IP allow-list, audit.
4. Admin APIs: stats; chunked upload (init/chunk/complete, resumable) → mapping preview/dry run → commit → job status; uploads history; notify (once per upload); outbox status/pause; settings CRUD; blocklist; removals; users; audit; backup now; cache purge; read-only toggle.
5. Outbox worker: campaigns, rendering at send time, backoff, quota, one-click unsubscribe (RFC 8058), bounce webhooks.
6. Removal flow: OTP-verified self-service delete + suppress; manual queue.

## Phase 6 — Frontend (Impeccable: PRODUCT.md → DESIGN.md → build → audit/polish)
1. PRODUCT.md + DESIGN.md (tokens: color, type, spacing, states; light + dark).
2. `index.html` (SSR template: SEO, JSON-LD, FAQ, noscript) + `app.css` + `app.js` (vanilla, < 25 KB gz):
   filter rail/drawer with accessible multi-select comboboxes (search, counts, (Blank), chips), hash-synced canonical filters, debounce + stale guard, skeleton loading, results table (desktop) / cards (mobile), NEW batch tag, "only newest 10", copy buttons, CSV export, inline sign-in (email → OTP with `autocomplete=one-time-code`), sponsor-break panel / Smart Link flow with visibility timer, ad-blocker nudge, donation block (UPI / PayPal / crypto + QR + copy), WhatsApp CTA with pre-filled filter summary, consent banner, BroadcastChannel tab sync, offline/429/503 states.
3. Legal pages (privacy, terms), removal page, unsubscribe page.
4. Admin UI (Operate mode, strict CSP): login, dashboard, upload wizard, imports, notify, settings forms (live preview of donation/WhatsApp blocks), blocklist, removals, users, audit.

## Phase 7 — SEO
Top-N page selection job; country / industry / country×industry templates; `410` below threshold; sitemap.xml; llms.txt; OG/Twitter tags; BreadcrumbList; Dataset JSON-LD with live counts. *Test:* acceptance #12 (structure checks; Lighthouse run in RUNBOOK).

## Phase 8 — Operations
Scheduler (no-overlap, `job_runs`); cleanup jobs; disk guard (80/90/95); backup (`VACUUM INTO` + zstd + retention + space check); alerts; `/healthz` `/readyz`; graceful shutdown. Deploy kit: `Caddyfile` (Origin CA cert, trusted_proxies Cloudflare, encode, `lb_try_duration`), hardened `dials.service`, `provision.sh` (ufw Cloudflare-only 443, fail2ban, swap + zram, sysctl, unattended-upgrades, journald cap, logrotate), `deploy.sh` (build → upload → swap → readyz gate → rollback), `cloudflare.md` (Origin Rule, Redirect Rule, Cache Rules, WAF rule, Turnstile), RUNBOOK.md.
*Test:* acceptance #11 (disk guard thresholds with a fake disk stat).

## Phase 9 — Review (Reviewer)
`code-review-graph build` → impact/blast-radius map; senior review checklist: correctness of claim/dedupe/facet logic, concurrency, error paths, security (authn/z, CSRF, XSS, injection, SSRF, uploads), resource limits, logging of PII (none at WARN), and fixes with regression tests.

## Phase 10 — Optimize (Optimizer)
Generate 1 M rows → import benchmark → `EXPLAIN QUERY PLAN` for the hot queries → index/plan tuning → Go benchmarks for preview/facets/claim → heap profile under `GOMEMLIMIT=150MiB` → page-weight audit → final report with measured numbers.

---

## Acceptance tests → where they're verified

| # | Test | Verified by |
|---|---|---|
| 1 | USA/usa/" USA " = one option; counts sum | `importer` + `search` tests |
| 2 | India OR USA × Software; removing filters never errors | `search` tests |
| 3 | (Blank) state → only empty-state rows | `search` tests |
| 4 | `ac me` matches Acme; injection/emoji safe | `filter` fuzz + `search` tests |
| 5 | facet counts ignore own dim (brute-force oracle) | `search` property test |
| 6 | two tabs → 20 distinct | `search.Claim` concurrency test |
| 7 | filter change → no re-issue | `search.Claim` test |
| 8 | disposable/bad rejected; OTP lockout | `emailcheck` + `auth` tests |
| 9 | early/reused/foreign token rejected | `ads` tests |
| 10 | re-upload → 0 new; notify count = `uploads.added` | `importer` + `mail` tests |
| 11 | disk guard → read-only + alert | `jobs` test (fake statfs) |
| 12 | SEO structure, sitemap, llms.txt reachable | `httpapi` tests + Lighthouse (RUNBOOK) |

---

## Inputs needed from the founder (none block the build; all have safe defaults)

| # | Item | Where it goes | Default until provided |
|---|---|---|---|
| 1 | WhatsApp number (with country code) + default message | Superadmin → WhatsApp | block hidden |
| 2 | UPI VPA + payee name | Superadmin → Donations | hidden |
| 3 | PayPal.me handle / link | Superadmin → Donations | hidden |
| 4 | Crypto networks to show for `0x6db1…fb` | Superadmin → Donations | Ethereum, BNB Smart Chain, Polygon, Base |
| 5 | Adsterra unit codes (banner 728×90 / 320×50, native or 300×250, optional Social Bar) | Superadmin → Ads | slots collapse; Smart Link preset |
| 6 | Email provider API key (Brevo recommended) + SPF/DKIM/DMARC on markemindai.com | `/etc/dials/dials.env` | `log` provider (dev) |
| 7 | Turnstile site key + secret | env | disabled in dev only |
| 8 | Cloudflare Origin CA cert/key; optional API token for cache purge | VPS / env | — |
| 9 | Reacher URL + token (if you run one) | Superadmin → Email | built-in MX checks |
| 10 | Sample CSV header row | importer aliases | broad alias map |
| 11 | Legal entity name, postal address, privacy contact email | Superadmin → Site | placeholders flagged in admin |
| 12 | Logo / brand mark (SVG) | `web/static` | wordmark |
