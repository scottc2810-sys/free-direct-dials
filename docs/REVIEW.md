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
