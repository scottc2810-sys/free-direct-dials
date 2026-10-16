-- 0002: several contact databases ("stores"), see docs/ARCHITECTURE.md §4.6.

-- Registry of contact stores. id 0 is this database; others are Postgres
-- (e.g. Supabase free projects) or extra SQLite files. Connection strings are
-- sealed with AES-256-GCM under a key derived from SECRET_KEY.
CREATE TABLE stores(
  id        INTEGER PRIMARY KEY,
  name      TEXT    NOT NULL UNIQUE,
  kind      TEXT    NOT NULL,            -- local | sqlite | postgres
  dsn       TEXT    NOT NULL DEFAULT '', -- sealed
  host      TEXT    NOT NULL DEFAULT '', -- display only: user@host:port/db, never the password
  cap_bytes INTEGER NOT NULL DEFAULT 0,  -- quota (0 = none; the local disk has its own guard)
  readable  INTEGER NOT NULL DEFAULT 1,  -- serves searches and claims
  writable  INTEGER NOT NULL DEFAULT 1,  -- receives new contacts from uploads
  created   INTEGER NOT NULL,
  bytes     INTEGER NOT NULL DEFAULT 0,
  contacts  INTEGER NOT NULL DEFAULT 0,
  last_ok   INTEGER NOT NULL DEFAULT 0,
  last_err  TEXT    NOT NULL DEFAULT '',
  stats     TEXT    NOT NULL DEFAULT '{}', -- headline numbers, mirrored after every data change
  mirror_ver INTEGER NOT NULL DEFAULT 0    -- the store's data version store_dims reflects
);
INSERT INTO stores(id, name, kind, created) VALUES (0, 'This server', 'local', CAST(strftime('%s','now') AS INTEGER));

-- Local mirror of each remote store's per-value counts. Totals, single-filter
-- counts, plain dropdowns, stats and labels come from here, and a store whose
-- mirror has none of a filter's values is never asked: remote databases are
-- only queried when nothing local can answer.
CREATE TABLE store_dims(
  store_id INTEGER NOT NULL,
  dim      INTEGER NOT NULL,
  value_n  TEXT    NOT NULL,
  label    TEXT    NOT NULL,
  cnt      INTEGER NOT NULL,
  PRIMARY KEY(store_id, dim, value_n)
) WITHOUT ROWID;

-- Which stores hold each user's unlocks, and which batch range: the personal
-- list, counts and export query only those stores, newest first, and skip a
-- store whose batches can't reach the page being shown.
CREATE TABLE user_stores(
  user_id   INTEGER NOT NULL,
  store_id  INTEGER NOT NULL,
  n         INTEGER NOT NULL DEFAULT 0,
  min_batch INTEGER NOT NULL DEFAULT 0,
  max_batch INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(user_id, store_id)
) WITHOUT ROWID;
INSERT INTO user_stores(user_id, store_id, n, min_batch, max_batch)
  SELECT user_id, 0, COUNT(*), MIN(batch), MAX(batch) FROM unlocked GROUP BY user_id;

-- Unlock batches are numbered per user across every store.
ALTER TABLE users ADD COLUMN last_batch INTEGER NOT NULL DEFAULT 0;
UPDATE users SET last_batch = COALESCE((SELECT MAX(batch) FROM unlocked u WHERE u.user_id = users.id), 0);

-- Landing pages are keyed by normalised value: dictionary ids differ per store.
ALTER TABLE seo_pages ADD COLUMN country_v  TEXT NOT NULL DEFAULT '';
ALTER TABLE seo_pages ADD COLUMN industry_v TEXT NOT NULL DEFAULT '';
UPDATE seo_pages SET
  country_v  = COALESCE((SELECT value_n FROM dims WHERE dims.id = seo_pages.country  AND dims.id > 12), ''),
  industry_v = COALESCE((SELECT value_n FROM dims WHERE dims.id = seo_pages.industry AND dims.id > 12), '');
CREATE INDEX seo_pages_country ON seo_pages(country_v, kind);
CREATE INDEX seo_pages_industry ON seo_pages(industry_v, kind);

-- Which store(s) an upload wrote to.
ALTER TABLE uploads ADD COLUMN target TEXT NOT NULL DEFAULT '';
