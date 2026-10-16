# Senior review: findings and resolutions

> Reviewer phase, 2026-10-15. Method: code-review-graph structural map (70 files, 1,058 symbols, 10,850 edges) to find hot spots and blast radius; `go vet`, `staticcheck`, `deadcode`, `govulncheck`; fuzzing; a brute-force oracle for facet maths; real-browser end-to-end runs (desktop and 375 px mobile); a real Slintel export (203 rows, 47 columns); and a manual pass over every auth, money, upload and resource-limit path.
> Every finding below is **fixed with a regression test** unless marked otherwise.

| # | Sev | Area | Finding (failure scenario) | Resolution |
|---|---|---|---|---|
| 1 | **High** | Supply chain | Go 1.27.0 standard library had 12 vulnerabilities **reachable** from our code (net/http HTTP/2, crypto/tls, html/template, net/textproto). | Pinned `toolchain go1.27.2` in go.mod; `govulncheck` now reports 0 reachable. CI runs govulncheck on every push. |
| 2 | **High** | Import scale | Throughput collapsed as the DB outgrew the 32 MB writer cache: ~4,500 rows/s at 20 k rows, under ~1,100 rows/s average past 500 k (random B-tree writes on 12 filter indexes plus FTS, WAL checkpoint every 4 MB). A 1 M-row upload would take hours. | Bulk mode: 128 MB writer cache during import, 64 MB checkpoint interval, FTS merging paused then one `optimize`, filter indexes built once after the first load, 5,000-row transactions. Indexes and FTS settings are restored at every start, so a crash mid-import is harmless. Numbers in PERFORMANCE.md. |
| 3 | **High** | Auth availability | A failed email send still counted toward the 3-codes-per-hour limit. A 3-minute provider outage locked every affected visitor out of signing in for an hour. | The send is refunded when delivery fails. Test: `TestOTPSendFailureDoesNotConsumeAllowance`. |
| 4 | **High** | Memory | CSV export buffered up to 100 k rows (~30 MB) per request. Ten concurrent exports would breach `MemoryMax` on the 0.75 GB box and get the process OOM-killed. | Export streams to the client; at most 2 run at once (others get 503 + Retry-After). |
| 5 | **High** | UI | Component `display` rules overrode the HTML `hidden` attribute, so "Export CSV" and "Only newest" showed for anonymous visitors. Found in the real browser. | Global `[hidden]{display:none !important}`. |
| 6 | Med | Security | The superadmin IP allow-list was optional. | Mandatory in prod (`ADMIN_ALLOW_CIDRS`; `0.0.0.0/0` rejected); localhost-only in dev; enforced at Cloudflare WAF, Caddy and app; `admin.js/css` are IP-restricted too; denied hits are logged. Tests cover other IP, IPv6, a spoofed header from a non-proxy peer, and a stolen admin cookie used from another network. |
| 7 | Med | Data quality | The curly apostrophe in the dimension folder had been corrupted into mojibake, so "O’Fallon" and "O'Fallon" were two filter values. | Byte-escaped literal plus test. (`staticcheck` ST1018 caught it.) |
| 8 | Med | Data quality | The fuzzer found that `'blank`-style values weren't a canonicalisation fixed point (different cache keys and URLs for the same filter). | Blank markers are re-checked after punctuation folding. A 1.2 M-exec fuzz run is clean and the seed is kept as a regression test. |
| 9 | Med | SEO | Landing-page slugs came from display labels, which change when the most common spelling shifts, so URLs would change after imports and lose rankings. | Slugs come from the stable normalised value. |
| 10 | Med | Import mapping | A second "Email" column (renamed "Email (2)") wasn't recognised. Slintel's `lead_titles` / `lead_divison`, `company_*` columns and `Tech\|Category` lists weren't mapped. Excel's `'+1…` text marker stayed in phone numbers. Excel had silently turned the size range "10-19" into the date serial 43739. | Alias table extended, duplicate-header suffixes ignored, company location fills blank person location, tech categories stripped, apostrophe stripped, `styles.xml` date formats decoded ("10-19" recovered). The real export: 203/203 rows, 0 missing titles. |
| 11 | Med | Concurrency | Shared (singleflight) facet and preview computations ran on the first caller's context. If that visitor navigated away, everyone waiting got an error. | Detached context with its own 3 s query timeout. |
| 12 | Med | Disk | A 2 KB `company_description` and duplicate address columns would add ~2 GB at 1 M rows. | Ignored by default (the admin can re-enable them per upload). |
| 13 | Low | Dead code | `Filter.Summary`, `IsBlank`, `IsNoRows` were unused. | Removed (`deadcode` is clean). |
| 14 | Low | Copy | "1 skipped: empty row 1" and "Queued 1 emails". | Fixed. |
| 15 | Info | Supply chain | `golang.org/x/crypto` advisory GO-2026-5932: no upstream fix, and **not reachable** from our code. | Accepted. Re-check when a fix ships (CI flags it). |

## Round 2 (2026-10-16): multiple databases, paging, rewarded ads

| # | Sev | Area | Finding | Resolution |
|---|---|---|---|---|
| 16 | **High** | Ads | Google's rewarded overlay rewrites the URL hash to `#goog_rewarded`. The page treated that as a filter change: filters reset mid-video and the claim could run on *no filter*. Found by playing Google's test rewarded unit in the browser. | `#goog_*` hashes are ignored, filters are restored to the address bar afterwards, and the claim always uses the visitor's filter. Re-verified: 10 Germany contacts for a Germany filter. |
| 17 | **High** | Data | Several databases: one person could be imported into two stores and handed out twice. | The import router checks every non-empty store for existing identities and merges there. Uploads refuse to run while any store is unreachable. Test: re-importing the whole file into the other store adds 0. |
| 18 | **High** | Compliance | A removal had to reach every store, and a failed one could be undone by a later upload. | Suppression keys are written first, then contacts are deleted in every store. If a store is down, the self-service request is queued for the admin with the real address. Test: removal reaches every store and a re-upload is suppressed. |
| 19 | Med | Performance / cost | The first federation design queried every database on every request (Supabase compute and egress). | Local mirror, memo per store version, store pruning, sequential background search, per-user store routing. Test: 0 remote calls for totals, single filters, plain dropdowns, stats, labels and first pages. |
| 20 | Med | Privacy | Supabase connection strings hold a password. | AES-256-GCM sealed under a key derived from `SECRET_KEY`; never returned to the browser; driver errors are password-redacted; `sslmode=disable` refused off localhost. |
| 21 | Med | Users | Deleting a user left unlocks in remote stores; SQLite can reuse the highest row id, so a new user could inherit them. | Unlocks are forgotten in every store holding them *before* the user row is deleted. If a store is down, the delete waits. |
| 22 | Low | UI | An empty partner slot printed the word "null" (DOM `append(null)`). | It renders nothing. |
| 24 | Med | Ads | VAST video: when the IMA SDK only loaded after the Play click, the ad container wasn't initialised inside the click, so browsers blocked unmuted autoplay and every tag "failed" into the banner break. Found in the browser. | The SDK preloads when the break opens and initialises synchronously in the click; the video falls back to muted when the browser reports no user activation. Each tag's failure reason is logged to the console. Re-verified with Google's sample tag. |
| 25 | Low | Ads | The IMA player's stylesheet/font was blocked by the page CSP. | Video modes allow https styles, fonts and `blob:` media. |
| 27 | Med | Search quality | "CEO" didn't find "Chief Executive Officer", plurals ("vice presidents") and filler words ("head of sales") missed matches, and Seniority/Department showed "VP" and "Vice President" as separate filters. The real Slintel export also put lists ("CXO, VP") and a few ZIP codes into Seniority. | Title synonym rewrite (both directions), singular prefixes, stop words, dotted abbreviations; dimension alias tables; single-choice dimensions take the first listed value; letterless values in text dimensions are blank; a one-time merge per store (dev data: Seniority went from 27 values to 7). Tests: canonical forms and idempotence, end-to-end matching on imported titles, cross-store merge; 7 M fuzz runs. |
| 26 | Med | Caching | CI fuzzing (Linux) found a technology filter whose 50-character cut ended right after a space. The trailing space was dropped on the next parse, giving two cache/CDN keys and URLs for one filter. | Trim after truncating; the failing input is kept as a regression seed. 2.2 M local fuzz executions clean. |
| 23 | Info | Caching | Complete public responses allow 30 s stale-while-revalidate, so a browser may briefly show the previous answer after an admin change. | Accepted (public data). Pending answers never use it, so live updates always reach the server. |

Verified in the browser (dev server):
- Preview break → page 2 auto-opens with rows 11–20.
- Sign-in → 10 s sponsor break → 10 contacts revealed with no click.
- Google test rewarded video → reward → 10 contacts (filter kept).
- A second database added from Superadmin → an upload split 25 new rows into it and merged the 1 existing person in place.
- Detach is refused while the database still serves searches.
- Validation: Google rewarded mode without an ad unit; ads.txt containing markup.
- ads.txt is served at `/ads.txt`.
- Partner card live; mobile (375 px) has no horizontal overflow.

## Impeccable frontend audit (directory page)

Measured in the browser (light + dark, 375 / 731 / 1024 / 1280 px, emulated touch) plus `impeccable detect` on the rendered page.

| Dimension | Before | After | What changed |
|---|---|---|---|
| Accessibility | 3 | 4 | Skip link becomes visible on focus. All touch targets ≥ 44 px on touch screens (chip remove 36 px, above WCAG 2.2 AA's 24 px). Contrast passes AA in both themes, measured over every text node. |
| Performance | 3 | 3 | First view ≈ 25 KB compressed (HTML 12 KB, CSS 24 KB, JS 47 KB raw). The shimmer stops under reduced motion. JS minification would add ~30 % (left as is: Caddy/Cloudflare compress, and readable source is easier to patch on a VPS). |
| Theming | 3 | 4 | The WhatsApp brand colour is a token. No other hard-coded colours outside the QR code's required white. |
| Responsive | 3 | 4 | Fixed (during the audit pass) a horizontal overflow at 721–960 px (a grid `1fr` minimum widened the page). Cards from 860 px down. The table scrolls inside its own box. |
| Implementation integrity | 3 | 4 | Decorative repeating stripes removed from the house ad and progress track. Green-bar banding stays only on table rows, where it aids scanning. The remaining detector item (FAQ "cramped padding") is a verified false positive. |
| **Total** | **15/20** | **19/20** | |

## Verified invariants (tests that must stay green)

- Acceptance 1–12 from the MVP spec (see PLAN.md for where each lives).
- Facet counts equal a brute-force in-memory evaluation over 150 random filter combinations.
- Three concurrent claims → 30 distinct contacts, zero overlap; filter changes never re-issue a contact.
- Public responses never set cookies (a CDN can't leak one visitor's session to another); personal responses are `no-store`.
- Encrypted backups: wrong key, tampered byte and truncation are all rejected.
- OTP: 5 wrong codes then locked; codes are single-use; send limits survive verification.
- Admin: TOTP replay rejected; lockout with backoff; CSRF on every mutation.

## Accepted risks and follow-ups

| Risk | Why accepted | Follow-up |
|---|---|---|
| "Require ads" ad-block policy is client-side only | A server can't see whether an ad rendered | Keep the default at *nudge* |
| Per-IP daily unlock counter is in memory | It resets on restart (deploys); the per-user cap is persistent | Persist it if abuse appears |
| Brevo/Resend free caps (300 / 100 a day) | They're enough at launch | Add SES via `smtp:` when subscribers pass ~2,000 (RUNBOOK §3) |
| Legal copy is a template | It must come from counsel | Review privacy/terms before launch |
| Race detector not run on Windows (needs cgo) | CI runs `go test -race` on Linux | — |
