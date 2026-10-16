# Free Direct Dials — System Architecture

> Owner: Architect · Status: **v1 approved for build** · Date: 2026-10-15
> Target: `https://markemindai.com/free-direct-dials/` on a 0.75 GB RAM / 20 GB disk VPS.
> Companion docs: [PLAN.md](PLAN.md) (build plan) · [RUNBOOK.md](RUNBOOK.md) (operations) · [PRODUCT.md](PRODUCT.md) (product/design brief)

---

## 0. Decisions locked with the founder (2026-10-15)

| Topic | Decision |
|---|---|
| Ad unlock flow | **Both modes, switchable from superadmin.** Default = *Sponsor break* (10 s in-page break showing Adsterra display/native units, no click required). Alt = *Smart Link* (opens `https://asiafilm.org/4/da200810262f12a03472e20ac1c98e2e` in a new tab, unlock after ≥10 s away). Reason: Adsterra bans incentivized traffic; a click-to-unlock Smart Link risks the account. |
| Hosting | Main site `markemindai.com` lives **elsewhere**. A Cloudflare **Origin Rule** sends only `/free-direct-dials*` to this VPS. |
| Donations | Crypto (EVM address `0x6db11d0a652de0ee57edaddfa70e5dac87d33bfb`), **UPI**, **PayPal**, all editable in superadmin. |
| Bulk/custom data | **WhatsApp** CTA (`wa.me`) with a pre-filled message describing the visitor's current filters; editable in superadmin. |
| Stack | One Go binary + SQLite (WAL) + Caddy, behind Cloudflare. No Docker. |
| Smart Link placements | Used "wherever possible", but always **labelled "Sponsored", `rel="sponsored noopener"`, new tab**: header, sponsor-break button, under results, empty / all-unlocked / daily-limit states, fallback banner in any ad slot without Adsterra code, footer, SEO pages. Each placement toggles in superadmin. Never in emails or on legal / removal / unsubscribe pages. |
| Superadmin access | **IP allow-list is mandatory** (`ADMIN_ALLOW_CIDRS`; the server refuses to start in prod without it) and enforced in 3 layers: Cloudflare WAF → Caddy `client_ip` → app (404 for everyone else, including `admin.js`/`admin.css`). Then password (argon2id), TOTP and CSRF. |
| Data uploads | CSV / TSV / semicolon CSV / `.csv.gz` / **Excel `.xlsx`** (streaming, sheet picker), up to 5 GB in resumable 8 MB chunks. Per-column mapping UI with samples, a 200-row **dry run** showing exactly what each row becomes, warnings, and **remembered mappings** per header layout. Profiled against a real Slintel export. |
| Contact databases (2026-10-16) | **Several stores**: the VPS SQLite database (always first, also the control plane) plus any number of **Supabase / Postgres** databases added in Superadmin → Databases (connection strings sealed with AES-256-GCM). Reads are **local-first and on demand**: a remote store is only queried when no local summary can answer, one store at a time, and the next store is searched **in the background** when the earlier ones have no more matches. Counts show "N+" until it answers. Every store is pinged every 6 h (keeps free projects awake). See §4.6. |
| Unlock modes (2026-10-16) | Sponsor break (default) · **VAST video break** (any network's VAST tags in a waterfall, played by Google's IMA SDK; unlock on complete/skip; banner break when nothing fills) · **Google Ad Manager rewarded video** (falls back to the sponsor break when Google has no video) · Smart Link. Contacts are revealed **the moment the ad completes**, with no extra click. Preview pages 2–5 each open after a 5 s sponsor break. Labelled **partner (affiliate) offers** sit in the high-intent states. |
| Look | Royal blue (`#2a4fd6`, `#7b9bff` at night); WhatsApp keeps its brand green. WhatsApp default **+91 98343 46179**. |
| Data security | DB files 0600 / dir 0700, `secure_delete=FAST` (erased people are zeroed in-page), **AES-256-GCM encrypted backups** (`BACKUP_KEY`), patched Go toolchain pinned (`go1.27.2`). |

---

## 1. Topology

```
                    ┌──────────────────────────── Cloudflare (free) ────────────────────────────┐
 Visitor ──HTTPS──▶ │ DNS · TLS · DDoS · WAF rate-limit · Turnstile · Cache Rules · Origin Rule │
                    └───────────────┬──────────────────────────────────────┬────────────────────┘
                     path ≠ /free-direct-dials*                 path = /free-direct-dials*
                                    ▼                                      ▼
                         Main site (existing host)          VPS :443 (only Cloudflare IPs allowed)
                                                             Caddy (Origin CA cert, gzip/zstd,
                                                                    trusted_proxies=Cloudflare)
                                                                      │ 127.0.0.1:8081
                                                                      ▼
                                                    ┌──────── dials (single Go binary) ────────┐
                                                    │ HTTP API · embedded UI · SEO renderer     │
                                                    │ workers: outbox · scheduler · disk guard  │
                                                    │ import jobs · backups                     │
                                                    └───┬──────────────┬──────────────┬────────┘
                                                        ▼              ▼              ▼
                                                  SQLite (WAL)   Mail providers   Email verifier
                                                  /var/lib/dials Brevo │ Resend │  (built-in MX +
                                                                 SMTP (failover)   optional Reacher)
```

**Why it survives traffic on 0.75 GB:** about 95 % of requests are answered by Cloudflare's edge: the page, SEO pages, assets, and `GET` API calls with canonical query strings. The origin only sees cache misses, sign-ins, and unlocks, which come to about one write per ad.

---

## 2. Memory & disk budget

| Component | Budget | Notes |
|---|---|---|
| OS (Debian 12 / Ubuntu 24.04 minimal) | ~120 MB | no desktop, no Docker |
| Caddy | ~25 MB | |
| Go heap | ≤150 MB | `GOMEMLIMIT=150MiB`, `GOGC=50` |
| SQLite page caches | ~96 MB | **per-connection** caches: writer 32 MB + 4 readers × 16 MB. (The spec's 64 MB × 5 connections would have been 320 MB.) |
| SQLite mmap | 128 MB shared | file-backed and reclaimable; reads go straight from the OS page cache |
| systemd | `MemoryHigh=350M`, `MemoryMax=450M` | throttle before OOM |
| Safety | 1 GB swapfile + zram, `vm.swappiness=20` | |

Disk (20 GB): OS 3 GB · DB ≈ 2–3 GB at 2 M contacts (narrow + dictionary design, §4) · WAL ≤ 200 MB · VACUUM-INTO scratch (= DB size, transient) · 2 compressed backups ≈ 1.5 GB · logs ≤ 300 MB · **≥ 8 GB headroom**.

---

## 3. Process architecture (Go)

```
cmd/dials            CLI: serve | migrate | admin create|reset-totp | import <csv> | backup | gen <rows> | check
internal/
  config/            env config + strict validation (refuses to start in prod with unsafe config)
  platform/          clock (injectable), ids, slog logger, disk stats (unix/windows build tags)
  db/                writer pool (1 conn) + reader pool (4 conns), pragmas, embedded migrations
  normalize/         text fold (NFKD, strip marks, lower, collapse ws), blanks, country aliases, phone, LinkedIn key
  emailcheck/        syntax (RFC-practical + IDNA), disposable list (+subdomains +patterns), MX/null-MX, Reacher, verdict cache
  importer/          sniff (BOM/UTF-16/cp1252, delimiter), header aliases → mapping, row clean, batch upsert, progress
  filter/            canonical filter (parse/encode), validation limits, SQL WHERE builder, FTS sanitizer
  search/            count · facets · preview · unlock-claim · LRU (byte-bounded) + singleflight + read limiter
  auth/              OTP, sessions, admin auth (argon2id + TOTP), CSRF, Turnstile
  ads/               ad tokens, mode rules, claim validation, daily caps
  payments/          donation settings validators (UPI VPA, PayPal, EVM EIP-55, BTC/SOL/TRON), QR → SVG
  mail/              Provider interface (brevo, resend, smtp, log) · failover · quota w/ OTP reserve · campaigns/outbox worker
  seo/               top-N page selection, renderers, sitemap.xml, llms.txt
  jobs/              scheduler (no-overlap, jitter, job_runs), cleanup, disk guard, backup, alerts
  httpapi/           router, middleware (real-IP, rate limit, recover, security headers, cache policy, body limits)
  web/               embedded static UI (index, app.js/css, admin), html/template SEO + legal + email templates
```

Concurrency model:
- **One writer connection.** All writes are serialized, so a race can never hand the same contacts to two claims. Unlock claims also take a per-user mutex, so two tabs queue rather than collide.
- **Four reader connections** behind a semaphore (8 waiters max). Past that, the API returns `503 Retry-After: 5`. Every query runs with `context.WithTimeout(3s)`, and modernc's driver interrupts SQLite on cancel.
- **Imports** run as a background job, committing every 2,000 rows. Between batches the writer is yielded, so OTP and unlock writes wait at most about 1 s.
- **Graceful shutdown** on SIGTERM: stop accepting connections, drain for 10 s, stop workers, run `wal_checkpoint(TRUNCATE)`, close.

---

## 4. Data model (SQLite)

### 4.1 Key optimisation: narrow facts + dictionary-encoded dimensions
Every filter, count, and facet runs against a **narrow integer table** (`contacts`, about 30 B/row, so 2 M rows ≈ 60 MB and the hot set fits in RAM). The wide text lives in `contact_info` and is read only for the ≤10–500 rows actually shown. Dimension values are stored once in `dims` and referenced by integer id. Id `0` means *(Blank)*.

```sql
-- dictionary of normalised dimension values
CREATE TABLE dims(
  id INTEGER PRIMARY KEY,
  dim INTEGER NOT NULL,            -- 1=country 2=state 3=city 4=industry 5=sector 6=ctype 7=seniority
                                   -- 8=division 9=decision 10=csize 11=revenue 12=econf
  value_n TEXT NOT NULL,           -- folded: NFKD, marks stripped, lower, trimmed, single-spaced
  label TEXT NOT NULL,             -- most common raw spelling (recomputed after import)
  cnt INTEGER NOT NULL DEFAULT 0,  -- unfiltered count → zero-cost facets when no other filter is active
  UNIQUE(dim, value_n)
);
CREATE TABLE dim_spell(dim_id INTEGER, raw TEXT, cnt INTEGER, PRIMARY KEY(dim_id, raw)) WITHOUT ROWID;

-- narrow facts (all filtering happens here)
CREATE TABLE contacts(
  id INTEGER PRIMARY KEY,
  country INT NOT NULL DEFAULT 0, state INT NOT NULL DEFAULT 0, city INT NOT NULL DEFAULT 0,
  industry INT NOT NULL DEFAULT 0, sector INT NOT NULL DEFAULT 0, ctype INT NOT NULL DEFAULT 0,
  seniority INT NOT NULL DEFAULT 0, division INT NOT NULL DEFAULT 0, decision INT NOT NULL DEFAULT 0,
  csize INT NOT NULL DEFAULT 0, revenue INT NOT NULL DEFAULT 0, econf INT NOT NULL DEFAULT 0,
  has_email INT NOT NULL DEFAULT 0, has_phone INT NOT NULL DEFAULT 0
);
CREATE INDEX c_country ON contacts(country);   -- one index per dimension (12)

-- wide payload (read only for displayed rows)
CREATE TABLE contact_info(
  id INTEGER PRIMARY KEY REFERENCES contacts(id) ON DELETE CASCADE,
  email_key TEXT UNIQUE,           -- lower(trim(email)); NULL if none
  li_key TEXT UNIQUE,              -- normalised LinkedIn /in/<slug>; NULL if none
  h_key TEXT UNIQUE,               -- sha1(name|company|title|phone) fallback identity
  name TEXT, title TEXT, company TEXT, website TEXT, linkedin TEXT,
  email TEXT, phone TEXT,
  extra BLOB,                      -- remaining columns, zstd-compressed JSON
  updated INT
);
CREATE TABLE contact_tech(tech TEXT NOT NULL, contact_id INT NOT NULL, PRIMARY KEY(tech, contact_id)) WITHOUT ROWID;
CREATE INDEX ct_contact ON contact_tech(contact_id);
CREATE VIRTUAL TABLE fts USING fts5(txt, content='', contentless_delete=1,
  tokenize='unicode61 remove_diacritics 2', prefix='2 3');   -- rowid = contacts.id
```

### 4.2 Users, auth, monetisation
```sql
CREATE TABLE users(id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL, created INT, last_seen INT,
  subscribed INT DEFAULT 1, blocked INT DEFAULT 0, bounced INT DEFAULT 0, unsub_token TEXT UNIQUE);
CREATE TABLE otps(email TEXT PRIMARY KEY, hash TEXT, exp INT, tries INT DEFAULT 0, sends INT, window_start INT, purpose TEXT);
CREATE TABLE sessions(id TEXT PRIMARY KEY /* sha256 */, user_id INT, created INT, exp INT, last_refresh INT);
CREATE TABLE ad_tokens(id TEXT PRIMARY KEY, user_id INT, created INT, used INT DEFAULT 0, mode TEXT);
CREATE TABLE unlocked(user_id INT, contact_id INT, batch INT, ts INT, PRIMARY KEY(user_id, contact_id)) WITHOUT ROWID;
CREATE INDEX u_batch ON unlocked(user_id, batch);
CREATE TABLE user_daily(user_id INT, day INT, ads INT, PRIMARY KEY(user_id, day)) WITHOUT ROWID;
```

### 4.3 Platform tables
```sql
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT /* JSON */, updated INT, updated_by TEXT);
CREATE TABLE blocklist(domain TEXT PRIMARY KEY, source TEXT);
CREATE TABLE verdicts(email TEXT PRIMARY KEY, ok INT, reason TEXT, ts INT);         -- 30-day cache
CREATE TABLE mx_cache(domain TEXT PRIMARY KEY, ok INT, reason TEXT, ts INT);
CREATE TABLE suppress(k TEXT PRIMARY KEY, ts INT);       -- sha256 of email/linkedin/phone key; re-imports skip
CREATE TABLE removals(id INTEGER PRIMARY KEY, kind TEXT, value TEXT, note TEXT, status TEXT, ts INT);
CREATE TABLE uploads(id INTEGER PRIMARY KEY, ts INT, filename TEXT, rows INT, added INT, updated INT,
  skipped INT, skip_reasons TEXT, status TEXT, notified INT DEFAULT 0, by TEXT);
CREATE TABLE campaigns(id INTEGER PRIMARY KEY, upload_id INT, subject TEXT, body_tmpl TEXT, created INT, total INT);
CREATE TABLE outbox(id INTEGER PRIMARY KEY, campaign_id INT, user_id INT, status TEXT, attempts INT DEFAULT 0,
  next_at INT, sent_at INT, err TEXT);                    -- body rendered at send time (no HTML per row)
CREATE TABLE mail_quota(day INT, provider TEXT, sent INT, PRIMARY KEY(day, provider));
CREATE TABLE admins(id INTEGER PRIMARY KEY, username TEXT UNIQUE, pw_hash TEXT, totp_secret TEXT,
  failed INT DEFAULT 0, locked_until INT DEFAULT 0, created INT);
CREATE TABLE admin_sessions(id TEXT PRIMARY KEY, admin_id INT, created INT, exp INT, idle_exp INT, csrf TEXT, ip TEXT);
CREATE TABLE audit(id INTEGER PRIMARY KEY, ts INT, actor TEXT, ip TEXT, action TEXT, detail TEXT);
CREATE TABLE seo_pages(slug TEXT PRIMARY KEY, kind TEXT, country INT, industry INT, cnt INT, updated INT);
CREATE TABLE job_runs(name TEXT PRIMARY KEY, started INT, finished INT, ok INT, msg TEXT);
CREATE TABLE meta(k TEXT PRIMARY KEY, v TEXT);           -- schema_version, dataset_version, read_only
```

### 4.4 Identity & dedupe (accuracy contract for imports)
Matching runs in priority order: `email_key` → `li_key` → `h_key`. The first match is updated, and only *non-empty* incoming fields overwrite ("later row wins non-empty"). This means re-uploading a file whose rows have no email does **not** create duplicates. The spec's email-only dedupe would have.

---

### 4.6 Several contact databases (federation)

```
                   control plane (VPS SQLite): users · sessions · settings · ad tokens · quotas · suppression list
                                               stores registry · store_dims mirror · user_stores
                                                        │
        ┌───────────────────────────────┬───────────────┴───────────────┬──────────────────────────────┐
  store 0 (VPS SQLite)          store 1 (Supabase #1)          store 2 (Supabase #2)          …
  contacts · dims · FTS5         schema fdd: contacts · dims ·   (same)
  unlocked (its contacts)        contact_info (tsvector GIN) · unlocked
```

- **Each store holds its own contacts and the unlocks of those contacts.** A contact lives in exactly one store; public ids are `storeID<<40 | localID` (exact in JavaScript).
- **Local-first, on demand.** After every import or removal, a remote store's per-value counts and headline stats are copied into `store_dims` / `stores.stats` (the *mirror*). Totals, single-filter counts, plain dropdowns, stats, SEO labels and the decision "this store can't match" come from the mirror, with no network call. Remote answers that must be asked (multi-filter counts, facets, rows) are memoised per store version.
- **Background search.** Preview rows are taken from store 0, then the next store only as far as the page reaches. When the stores so far are used up, the next is searched in the background (at most two background searches at a time). A request waits 1.2 s, then answers with what it has, marked `pending`. The page shows "12,340+ … Searching more databases" and polls until it is complete (pending answers are cached for ~1.5 s only).
- **Claims** go store by store in order. A store that runs out for a visitor and filter is remembered (until its data changes) and skipped next time, and the next store is warmed in the background. The ad was already watched, so a claim waits for whatever store it needs.
- **Personal list.** `user_stores` records which stores hold each user's unlocks and their batch range. The keyset-paged view (batch desc, store, id) asks only those stores, newest batches first, and leaves a store whose newest batch is older than the page for the next page.
- **Imports** route through `store.Router`: identities already present in another store merge there (`ExistingKeys`, skipped for empty stores); new people go to the chosen store, or with *Auto* to the first writable store under 90 % of its quota. Uploads and removals refuse to run while a store is unreachable, so nothing can be duplicated or left behind. Removal suppression keys are written to the control plane *before* deleting.
- **Postgres store** (`internal/store/postgres.go`): pgx pool of 4, no named prepared statements (works through Supavisor in session or transaction mode), TLS mandatory, 8 s query timeout, bounded concurrency. Batches upsert with a handful of `unnest()` statements and the same merge rules as SQLite (`importer.Merge`). Identity keys are unique constraints checked at commit. Keyword and technology search use `tsvector` columns built from the app's own tokenizer (`filter.IndexTokens` / `TechTokens`), queried as literal prefix lexemes, so results match SQLite FTS5.
- **Failure handling.** A store that errors is skipped with exponential back-off (5 s → 5 min); answers become `partial`. The 6-hourly monitor reconnects it, writes a keepalive, refreshes size, contacts and mirror, and alerts at 80 % of quota or when it is down.
- **Verified** by `internal/store/store_test.go`: three SQLite stores give exactly the same counts, facets, preview pages, claims and paging as one database holding the same rows. Further tests cover cross-store re-import (0 added) and removal reaching every store, plus partial results and the circuit breaker, mirror-only answers (0 remote calls), and pending/background completion. The same suite runs against a real Postgres when `DIALS_TEST_PG_DSN` is set.

## 5. Filter engine — accuracy contract

| Rule | Implementation |
|---|---|
| Normalisation | `fold(s)` = NFKD → strip combining marks → lower → trim → collapse whitespace. Blanks: `''`, `n/a`, `na`, `null`, `none`, `-`, `--`, `unknown`, `not available`, `0` *(only for dims)* → id 0. Country aliases (`us`, `usa`, `u.s.a.`, `united states of america` → `united states`; `uk`, `great britain` → `united kingdom`; `uae` …). |
| Label | Most common raw spelling per `dims.id` (from `dim_spell`). |
| Semantics | OR within a dimension (`col IN (…)`), AND across dimensions. |
| Blank | `__blank__` token → id 0 → shown as **(Blank)**. |
| Keyword | Split on whitespace, keep `[\p{L}\p{N}]` runs only, so all FTS operators/quotes/emoji are dropped. Each token becomes `"tok"*` and tokens are ANDed. Max 10 tokens, 200 chars. An empty result after sanitising means no keyword filter. Never a syntax error. |
| Technology | Comma list ≤ 10, lowercased, prefix match with an index range scan `tech >= 'hub' AND tech < 'huc'`, OR'd. |
| Has email / phone | Flags computed at import: syntax valid + not disposable; phone needs ≥6 digits. |
| Facets | For dimension X: all filters **except X**, then `GROUP BY X`, top 300, plus the selected values always included. **Fast path:** if no other filter is active, read `dims.cnt` directly (no scan). `q=` searches labels for long-tail values such as cities. |
| Totals | `COUNT(*)` on the narrow table, served from the cache. |
| Canonical filter | Sorted keys, sorted de-duplicated values, folded, empty/false dropped. Limits: keyword 200 chars, 50 values per dimension, 4 KB total. The same string serves as the cache key, the CDN cache key, and the shareable `#` URL. Unknown values match nothing; they never cause an error. |
| Stale responses | Client attaches a monotonically increasing request id and ignores out-of-order replies. Debounce 300 ms. |

---

## 6. Unlock & monetisation flow

```
Browse (anonymous, cached)          Sign in (inline, no modal)                   Unlock
─────────────────────────────       ──────────────────────────────────────       ───────────────────────────────────────────
filters → count + masked preview →  email → Turnstile → checks:                  POST /api/ad/start → token{mode,min_s}
                                    syntax · blocklist(+subdomains, patterns) ·  mode=sponsor: inline 10 s break with ad slot
                                    business-email rule (opt) · MX/null-MX ·     mode=smartlink: open Smart Link in new tab;
                                    Reacher (opt, 20 s, conc 3, fail-open)       hidden-time ≥ min_s (visibility API)
                                    → OTP (HMAC, 10 min, 5 tries, 3/h/email,    POST /api/ad/claim{token,f}
                                      10/h/IP) → session cookie (30 d, sliding)  server: token unused · same user · age ≥ min_s−1
                                                                                  · daily cap · tx: next 10 NEVER-unlocked
                                                                                  matching contacts ORDER BY id → batch=max+1
```

- **VAST video break.** Up to 5 tag URLs are tried in order (8 s request timeout each, 10 s to start). Common macros are expanded. The IMA ad container is initialised inside the visitor's *Play* click so sound is allowed, and the video plays muted when the browser reports no user activation. The claim fires on `COMPLETE`/`SKIPPED`/`ALL_ADS_COMPLETED` after `STARTED`, with a 5 s server floor (the same as rewarded). With no fill anywhere, the same token runs the banner sponsor break. In video modes the CSP admits third-party scripts, styles, fonts and `blob:` media.
- **Reveal on completion.** Sponsor break: the claim fires automatically when the visible-time countdown ends. Google rewarded (GPT `defineOutOfPageSlot(..., REWARDED)`): the visitor opts in, and the claim fires on `rewardedSlotGranted`. If GPT has no slot, no fill, or is blocked, or nothing arrives within 8 s, the same token becomes a sponsor break. The server floor for rewarded tokens is 5 s (web rewarded has no server-side verification).
- **Preview paging.** `/api/preview?page=0..4` returns 10 masked rows per page, with the depth set by Limits → preview pages. Pages after the first open after a short sponsor break (once per page per tab). Signed-in visitors page through their unlocked contacts with keyset cursors (`/api/me/view?after=`), 50 per page.
- **Never repeats, never double-hands.** A per-user mutex, the `PRIMARY KEY(user_id, contact_id)` in each store, and the fact that each contact lives in one store together guarantee it. Acceptance tests 6 and 7 verify this, and so does the federation test across stores.
- **Anti-scrape:** each user gets at most N ads per day (admin setting, default 50 = 500 contacts). Claims also have a per-IP daily cap and a minimum interval of `min_s` between them.
- **Ad blockers:** policy is set by the admin (`allow` | `nudge` (default) | `require`). Detection uses a `no-cors` fetch to the configured ad script host.
- **Ad slots** (top banner 90/50 px, in-content 250 px, sponsor break 250 px, footer) have fixed heights, so they cause no layout shift. Snippets are pasted in admin and rendered inside **sandboxed `srcdoc` iframes** by default. Admin can switch a slot to *direct* when fill rates need it. A global *Social Bar* snippet can only run direct and has an explicit warning.

---

## 7. Donations & WhatsApp (superadmin-managed)

Settings are JSON stored in `settings`, validated on save, written to the audit log, and exposed through `GET /api/config` (cached 60 s; optional Cloudflare purge on save).

| Method | Stored | Validation | Rendering |
|---|---|---|---|
| UPI | VPA, payee name | `^[a-zA-Z0-9._-]{2,256}@[a-zA-Z][a-zA-Z0-9]{1,63}$` | On mobile, a "Pay with UPI app" button (`upi://pay?pa=&pn=&cu=INR`). On desktop, a QR code. Both get a copy button. |
| PayPal | paypal.me handle or link | host ∈ {paypal.me, www.paypal.com}, https | button |
| Crypto | list of wallets {network, tokens, address} | EVM: hex + **EIP-55 checksum** (rejects mistyped addresses); BTC (bech32/base58), SOL, TRON patterns | address, QR, copy button, per-network chips, and a "send only on these networks" warning |
| Buy Me a Coffee / other | URL | https only | link button |
| WhatsApp | E.164 digits, default message, toggle | 8–15 digits | `https://wa.me/<n>?text=` with the visitor's filters + match count pre-filled; shown in the header, empty states, "all unlocked", the daily-limit state, and the footer |

Every block hides itself when it's empty or disabled. That means you can launch before the UPI, PayPal, or WhatsApp values are known.

---

## 8. Admin (superadmin) security

1. Secret path (`ADMIN_PATH`, ≥ 24 random characters). Any other path returns 404 with no hints.
2. Optional IP allow-list (`ADMIN_ALLOW_CIDRS`). Client IP comes from `CF-Connecting-IP`, rewritten by Caddy, which trusts only Cloudflare ranges.
3. Login uses username, an **argon2id** password, and mandatory **TOTP** (RFC 6238 with a ±1 step window and replay protection). Lockout uses exponential backoff, 5 failures per 15 min per IP and per account.
4. Admin session cookie `fdd_a` is `HttpOnly; Secure; SameSite=Strict`, with a 30 min idle timeout and 12 h absolute. Every mutation is protected by a per-session **CSRF** token.
5. Strict CSP on admin pages (`script-src 'self'`, no ads, no third parties). Every action goes to the `audit` log.
6. An optional Caddy `basic_auth` layer can sit in front for defence in depth.

Capabilities: stats dashboard, chunked resumable CSV upload (**works past Cloudflare's 100 MB request limit**), mapping preview and dry run, import progress, imports history, **Notify subscribers** (once per upload), outbox controls, settings (Ads · Donations · WhatsApp · Limits · Email · Site/maintenance), blocklist, removal requests, user search/block/delete, audit log, backup now, cache purge, read-only toggle.

---

## 9. Email

- A `Provider` interface with **Brevo**, **Resend**, **SMTP** (works for SES, Zoho, Postmark and similar), and **log** (dev) implementations. Failover order is configurable.
- **Quota with OTP reserve:** notification sends stop at `daily_cap − reserve` (default reserve 30 %), so a campaign can never block sign-ins.
- **Campaigns and outbox:** the template is stored once and each email is rendered at send time. The spec's per-row HTML would have been about 10 GB at 2 M rows. Retries back off exponentially, at most 5 attempts. A 4xx response is a permanent failure and marks the user `bounced`.
- **Unsubscribe:** sends the `List-Unsubscribe` and `List-Unsubscribe-Post: List-Unsubscribe=One-Click` headers (RFC 8058, required by Gmail and Yahoo). A GET request shows a confirmation page, because link scanners such as Outlook SafeLinks would otherwise unsubscribe people. The POST does the unsubscribe.
- **Bounce/complaint webhooks:** `POST /api/hooks/mail/{provider}?k=SECRET` sets `users.bounced`.
- The footer has the donate link, the unsubscribe link, and the legal postal address (CAN-SPAM).

---

## 10. Caching

| Path | Origin `Cache-Control` | Cloudflare rule |
|---|---|---|
| `/assets/*` (content-hashed names) | `public, max-age=31536000, immutable` | Cache everything |
| `/`, `/country/*`, `/industry/*`, legal pages, `sitemap.xml`, `llms.txt` | `public, max-age=0, s-maxage=600` | Eligible for cache; purge after import |
| `/api/config`, `/api/stats`, `/api/facets`, `/api/preview` | `public, max-age=0, s-maxage=60` | Eligible for cache (query string in key) |
| `/api/me/*`, `/api/ad/*`, `/api/auth/*`, admin | `private, no-store` | Bypass |

Federated answers carry their completeness: *complete* (5 min LRU / 60 s CDN), *partial* because a store is down (20 s / 20 s), and *pending* because a store is still being searched (≈1.5 s LRU / 1 s CDN, no stale-while-revalidate, so polls reach the server).

Public responses **never** read cookies or emit `Set-Cookie`. A test enforces this, so personal data can't leak through the CDN. On the origin, a byte-bounded LRU (30 MB) is keyed by `dataset_version + canonical filter` and backed by singleflight, so a burst of identical requests runs one query. Imports bump `dataset_version`, which invalidates the cache instantly.

---

## 11. SEO & LLM discoverability

- `index` is rendered from `html/template` at startup and again after each import. It carries the live counts, title, description, canonical, Open Graph tags, and JSON-LD (`WebApplication`, `Dataset`, `FAQPage`). The visible FAQ and a `<noscript>` summary are part of the page.
- **Collision-free routes:** `/free-direct-dials/country/{c}/`, `/industry/{i}/`, and `/country/{c}/{i}/`. The spec's `/{country}` and `/{industry}` would collide on names like "Georgia" or "Jordan".
- Pages are **rendered on demand** and cached at the edge for a day. There are no files on disk. A nightly job selects the top 500 pages by count. Pages that fall under 25 contacts return `410 Gone` so they get de-indexed quickly. Each page carries `BreadcrumbList`.
- `sitemap.xml` and `llms.txt` are served under `/free-direct-dials/`. **`robots.txt` and the root `llms.txt` belong to the main site.** Add `Sitemap: https://markemindai.com/free-direct-dials/sitemap.xml` to the main robots.txt, or submit the sitemap in Search Console (see RUNBOOK).
- Performance budget: under 60 KB gzip for the first view and LCP under 1.5 s. The page uses system fonts and no framework, and ads load after first paint.

---

## 12. Reliability, operations, observability

| Concern | Mechanism |
|---|---|
| Disk guard (every 10 min) | ≥80 %: alert email. ≥90 %: uploads, imports, and backups disabled. ≥95 %: all user writes rejected with friendly messages. Reads always keep working. |
| Backups (daily 03:30) | `VACUUM INTO` a scratch file, then streaming zstd at level 6 (low memory). Keeps 2 local copies. Runs only if free space ≥ 1.3 × DB + 1 GB. Optional offsite via rclone or Litestream to Cloudflare R2 (RUNBOOK). |
| Maintenance (daily 03:00) | `wal_checkpoint(TRUNCATE)`, `incremental_vacuum(2000)`, `PRAGMA optimize`, prune verdicts > 30 d, prune outbox > 7 d, expire OTPs, sessions, and tokens, refresh dims and SEO. |
| Monthly | `VACUUM` if freelist > 25 % and free disk ≥ 1.2 × DB. Prune unsubscribed users inactive > 24 months. Orphan sweep. |
| Health | `/healthz` (process) and `/readyz` (DB ping + writable + disk OK), checked by an uptime monitor. |
| Alerts | Email to `ADMIN_EMAIL`, rate-limited to 1 per type per 6 h. Triggers: disk, backup failure, mail provider failure, 5xx spike, stuck outbox. |
| Logs | `slog` JSON at WARN in prod, with request ids. journald capped at 100 MB. Caddy logs rotated daily, keep 3, 50 MB max. |
| Deploys | Cross-compile with `CGO_ENABLED=0`, then `scp`, an atomic symlink swap, `systemctl restart`, a `/readyz` gate, and **automatic rollback**. Caddy `lb_try_duration 5s` makes restarts invisible to visitors. |
| Migrations | Embedded, versioned, transactional, with an automatic pre-migration backup. |
| Origin protection | Firewall allows 443 only from Cloudflare IP ranges. Origin CA cert, Full (strict). Optional Authenticated Origin Pulls. |

---

## 13. Security checklist

- **Injection:** SQL is parameterized only, and FTS input is rebuilt from letter and digit tokens. The UI writes text through `textContent` only, and templates use `html/template`.
- **CSRF:** public POSTs need `SameSite=Lax`, a matching `Origin` check, a JSON content type, and an `X-FDD` header. Admin forms also carry a CSRF token.
- **Headers:** CSP (strict on admin, ad-compatible on public), `nosniff`, `Referrer-Policy`, `Permissions-Policy`, `frame-ancestors 'none'`.
- **Limits:** in-app per-IP token buckets (memory-bounded LRU), plus a Cloudflare WAF rate rule on `/api/auth/*`.
- **Timeouts:** `ReadHeaderTimeout 5s`, `MaxHeaderBytes 16KB`, a 64 KB JSON body cap, and 16 MB admin chunks.
- **Enumeration:** sign-up returns the same response whether or not the account exists. OTPs are compared in constant time.
- **Uploads:** server-generated file names, a gzip decompression cap, and disk checks mid-import. Files are deleted after import.
- **Secrets:** stored in `/etc/dials/dials.env` (0600). They never live in the DB or show in the admin UI, which shows "configured ✓" instead.
- **CSV export:** cells starting with `= + - @ \t \r` are prefixed with `'` to block formula injection.

---

## 14. Compliance

- Privacy Policy, Terms, and a **self-service removal page**. Removal verifies the person by OTP, then deletes the contact, adds it to the suppression list, and invalidates caches immediately. Requests without an email go to the admin queue (30-day SLA).
- The suppression list stores **hashes**, not the removed person's email. Re-uploads skip suppressed records.
- Consent banner for ads, with mode `all | eu_only` (detected by timezone) | `off`. Rejecting consent means ad scripts are not loaded.
- Admin must confirm on every upload that they have the right to publish the data. The confirmation is stored in the audit log.
- Every email carries the unsubscribe link, the one-click headers, and a postal address.

---

## 15. Corrections to the original MVP spec (Architect review)

| # | Spec said | Problem | Fix |
|---|---|---|---|
| 1 | `free-direct-dails` / `free-direct-dials` | typo, split SEO | canonical `/free-direct-dials`, 301 redirect from the typo |
| 2 | `cache_size=-65536` | per connection, so ×5 = 320 MB on a 750 MB box | 32 MB writer + 16 MB per reader, plus mmap |
| 3 | `outbox.html` per row | ~10 GB at 2 M users | campaigns + render at send time |
| 4 | `fts5(content='')` | contentless rows can't be deleted or updated | `contentless_delete=1` |
| 5 | `/{country}` and `/{industry}` | slug collisions (Georgia, Jordan…) | `/country/…`, `/industry/…` |
| 6 | static SEO files on disk | disk growth, regeneration cost | on-demand render + edge cache |
| 7 | dedupe on email only | rows without email duplicate on every re-upload | email → LinkedIn → hash identity |
| 8 | admin upload through Cloudflare | free plan caps request bodies at 100 MB | chunked resumable upload + `.csv.gz` + `dials import` CLI |
| 9 | "Caddy rate limit" | not built into Caddy | in-app limiter + Cloudflare rate rule |
| 10 | robots.txt / llms.txt at our root | the root belongs to the main site | sub-path files + main-site instructions |
| 11 | GET unsubscribe | link scanners unsubscribe people | confirmation page + RFC 8058 one-click POST |
| 12 | 100–300 emails/day | a campaign can starve OTP mail; doesn't scale | OTP reserve, failover, SMTP/SES adapter |
| 13 | Smart Link as reward | Adsterra incentivized-traffic ban | switchable modes, default sponsor break |
| 14 | facets on a wide text table | slow scans on 1 vCPU | narrow int table + dictionary dims + `dims.cnt` fast path |
| 15 | dropdown shows top 300 values only | long-tail cities unreachable | server-side value search `q=` |
| 16 | `zstd -19` weekly | memory-heavy on 750 MB; a week of data loss | level 6 streaming, daily, offsite option |
| 17 | `MemoryMax=300M` + 256 MB mmap | page-cache thrash | `MemoryHigh=350M`/`MemoryMax=450M`, mmap 128 MB |
| 18 | ad snippets in the page | stored-XSS blast radius | sandboxed iframes by default |
| 19 | unlimited claims | whole-DB scraping | per-user/IP daily caps (admin-configurable) |
| 20 | session cookie on a shared domain | main site sees it | `Path=/free-direct-dials` |
| 21 | Has-email counted any email | disposable emails inflate counts | flag = syntax + non-disposable |
| 22 | weekly `ANALYZE` | stale plans after imports | `PRAGMA optimize` after each import + nightly |
