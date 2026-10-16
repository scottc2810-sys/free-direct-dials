# Performance

> Measured 2026-10-15 with `dials bench -plans` on **974,147 contacts** (1 M synthetic rows; 2.6 % merged as duplicates).
> Caches disabled (every query hits SQLite). Hardware: 4-core Windows dev machine with an SSD. A 1 vCPU VPS will be slower on CPU-bound scans, but almost all public traffic is answered by the Cloudflare edge and the in-process cache before it reaches SQLite.
> Re-run after big imports or schema changes: `dials bench -plans`.

## Visitor-facing queries (p50)

| Filter | Matches | Count | Preview (count + 10 rows) | Facet: country | Facet: city |
|---|---:|---:|---:|---:|---:|
| none (landing) | 974,147 | 0 ms | 0.5 ms | 0 ms | 0 ms |
| 1 dimension (fast path) | 232,162 | 0 ms | 38 ms | 0 ms | 30 ms |
| 2 dimensions | 39,326 | **4 ms** | **11 ms** | 20 ms | 352 ms |
| OR within a dimension + 1 more | 62,457 | 5 ms | 18 ms | 20 ms | 130 ms |
| 3 dimensions + has email | 2,916 | 31 ms | 61 ms | 192 ms | 175 ms |
| has email + has phone only | 848,804 | 73 ms | 82 ms | **101 ms** | **102 ms** |
| rare city | 46,182 | 0 ms | 8 ms | 81 ms | 0 ms |
| keyword, common prefix | 227,711 | 199 ms | 284 ms | 314 ms | 323 ms |
| keyword + country | 54,537 | 322 ms | 512 ms | 406 ms | 347 ms |
| technology prefix | 108,067 | 165 ms | 255 ms | 213 ms | 219 ms |
| technology + 2 dimensions + phone | 4,341 | 122 ms | 159 ms | 129 ms | 123 ms |
| facet value search (city "san") | — | — | — | — | 0 ms |

| Signed-in path | Result |
|---|---|
| Claim the next 10 (2 filters, 40 consecutive batches) | **p50 41 ms · p95 51 ms** |
| Personal view (400 unlocked) | 7 ms |
| Process memory during the benchmark | 72 MB (heap 2 MB) |

## Imports

| Run | Before optimisation | After |
|---|---:|---:|
| 300 k rows | 229 s (1,310 rows/s) | **87 s (3,450 rows/s)**, identical results |
| 1 M rows | still running after 18 min; on course for 40+ min | **6 min 16 s** |
| Real Slintel export (203 rows × 47 cols) | 860 KB DB | **680 KB** (−21 %; about −27 % per contact excluding fixed schema overhead) |

Database at 1 M synthetic contacts: **664 MB**, so the 20 GB disk plan has room for several million contacts. Real vendor data is wider; plan for about 2.4 KB per contact (measured on the Slintel sample).

## What made the difference

| Change | Effect (1 M rows) |
|---|---|
| Narrow integer `contacts` table + dictionary-encoded dimensions | Filters scan small rows; labels live once in `dims` |
| Every filter index carries `has_email, has_phone` (covering) | Facets under the email/phone toggles: **1.08–1.26 s → 0.10 s** |
| Composite indexes country×industry, industry×country, country×state, country×city | 2-filter count **126 ms → 3–4 ms**; industry facet given country **237 → 30 ms**; claim **143 → 41 ms** |
| Sampled `ANALYZE` after every import (`analysis_limit=2000`) | Without statistics the planner preferred a single-column index plus row lookups |
| `dims.cnt` fast path | Landing-page facets and single-filter counts never touch `contacts` |
| Technologies and keywords in contentless FTS5 with `detail=none` | A (technology, contact) pair costs ~1–3 B instead of ~50 B in a table (real data: 31.6 technologies per contact) |
| Bulk import mode: 128 MB writer cache, 64 MB WAL checkpoints, FTS merging paused then one `optimize`, filter indexes built once after an initial load, 5,000-row transactions | Import **2.6× faster** and no collapse as the DB grows |
| Heavy vendor columns ignored by default (descriptions, duplicate addresses, vendor ids) | ~2 GB less disk per million Slintel-style rows |

## Remaining hot spots (next candidates if needed)

1. **Keyword + another filter (320–510 ms).** The FTS result is materialised and then probed. If keyword search gets heavy use, keep a per-keyword cache of rowid lists or move short prefixes onto the cached path.
2. **City facet with two other filters (352 ms).** Group-by on a column outside the composite. Fix: a `(country, industry, city)` covering index, but only if analytics show visitors opening City after choosing country + industry (costs ~14 MB per million rows).
3. **Claim p95 51 ms.** The target was 50 ms. Claims are rate-limited by the sponsor break (≥ 10 s each), so this has no user-visible impact.

## Memory and I/O settings (production)

`GOMEMLIMIT=150MiB`, `GOGC=50`, systemd `MemoryHigh=350M` / `MemoryMax=450M`. SQLite: WAL, `synchronous=NORMAL`, writer cache 32 MB (128 MB during imports only), 4 readers × 16 MB, 128 MB shared mmap, `temp_store=MEMORY`, `journal_size_limit=64 MB`, `secure_delete=FAST`, incremental auto-vacuum.
