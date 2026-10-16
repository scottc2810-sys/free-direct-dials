-- 0001: initial schema. See docs/ARCHITECTURE.md §4.

CREATE TABLE meta(k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;

-- Dictionary of normalised dimension values. ids 1..12 are the (Blank) value of dims 1..12.
CREATE TABLE dims(
  id      INTEGER PRIMARY KEY,
  dim     INTEGER NOT NULL,
  value_n TEXT    NOT NULL,
  label   TEXT    NOT NULL,
  cnt     INTEGER NOT NULL DEFAULT 0,
  UNIQUE(dim, value_n)
);
CREATE INDEX dims_cnt ON dims(dim, cnt DESC);
INSERT INTO dims(id, dim, value_n, label) VALUES
  (1,1,'__blank__','(Blank)'),(2,2,'__blank__','(Blank)'),(3,3,'__blank__','(Blank)'),
  (4,4,'__blank__','(Blank)'),(5,5,'__blank__','(Blank)'),(6,6,'__blank__','(Blank)'),
  (7,7,'__blank__','(Blank)'),(8,8,'__blank__','(Blank)'),(9,9,'__blank__','(Blank)'),
  (10,10,'__blank__','(Blank)'),(11,11,'__blank__','(Blank)'),(12,12,'__blank__','(Blank)');

CREATE TABLE dim_spell(
  dim_id INTEGER NOT NULL,
  raw    TEXT    NOT NULL,
  cnt    INTEGER NOT NULL,
  PRIMARY KEY(dim_id, raw)
) WITHOUT ROWID;

-- Narrow facts table: every filter/count/facet runs here.
CREATE TABLE contacts(
  id        INTEGER PRIMARY KEY,
  country   INTEGER NOT NULL, state    INTEGER NOT NULL, city     INTEGER NOT NULL,
  industry  INTEGER NOT NULL, sector   INTEGER NOT NULL, ctype    INTEGER NOT NULL,
  seniority INTEGER NOT NULL, division INTEGER NOT NULL, decision INTEGER NOT NULL,
  csize     INTEGER NOT NULL, revenue  INTEGER NOT NULL, econf    INTEGER NOT NULL,
  has_email INTEGER NOT NULL DEFAULT 0,
  has_phone INTEGER NOT NULL DEFAULT 0
);
-- Filter indexes (covering, with the has_email/has_phone flags, plus composites for the
-- most common pairs) are created by db.EnsureIndexes: one definition, re-applied at every start.

-- Wide payload, read only for rows actually displayed.
CREATE TABLE contact_info(
  id        INTEGER PRIMARY KEY REFERENCES contacts(id) ON DELETE CASCADE,
  email_key TEXT UNIQUE,
  li_key    TEXT UNIQUE,
  h_key     TEXT UNIQUE,
  name TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', company TEXT NOT NULL DEFAULT '',
  website TEXT NOT NULL DEFAULT '', linkedin TEXT NOT NULL DEFAULT '',
  email TEXT NOT NULL DEFAULT '', phone TEXT NOT NULL DEFAULT '',
  extra BLOB,
  updated INTEGER NOT NULL
);

-- Search indexes are contentless with detail=none: we only ever AND/OR prefix terms (no
-- phrases), so positions are dead weight. Measured on a real Slintel export (31.6
-- technologies per contact) a (tech, contact) pair costs 1-3 bytes here vs ~50 in a table.
CREATE VIRTUAL TABLE fts USING fts5(
  txt, content='', contentless_delete=1, detail=none,
  tokenize='unicode61 remove_diacritics 2', prefix='2 3'
);
CREATE VIRTUAL TABLE tech_fts USING fts5(
  txt, content='', contentless_delete=1, detail=none,
  tokenize="unicode61 remove_diacritics 2 tokenchars '.+#-'", prefix='2 3'
);

-- Users & auth
CREATE TABLE users(
  id          INTEGER PRIMARY KEY,
  email       TEXT    NOT NULL UNIQUE,
  created     INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  subscribed  INTEGER NOT NULL DEFAULT 1,
  blocked     INTEGER NOT NULL DEFAULT 0,
  bounced     INTEGER NOT NULL DEFAULT 0,
  unsub_token TEXT    NOT NULL UNIQUE
);
CREATE TABLE otps(
  email        TEXT    NOT NULL,
  purpose      TEXT    NOT NULL,
  hash         TEXT    NOT NULL,
  exp          INTEGER NOT NULL,
  tries        INTEGER NOT NULL DEFAULT 0,
  sends        INTEGER NOT NULL DEFAULT 0,
  window_start INTEGER NOT NULL,
  PRIMARY KEY(email, purpose)
) WITHOUT ROWID;
CREATE TABLE sessions(
  id           TEXT PRIMARY KEY,
  user_id      INTEGER NOT NULL,
  created      INTEGER NOT NULL,
  exp          INTEGER NOT NULL,
  last_refresh INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_exp ON sessions(exp);

-- Monetisation
CREATE TABLE ad_tokens(
  id      TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  created INTEGER NOT NULL,
  used    INTEGER NOT NULL DEFAULT 0,
  mode    TEXT    NOT NULL
) WITHOUT ROWID;
CREATE INDEX ad_tokens_user ON ad_tokens(user_id, created);
CREATE TABLE unlocked(
  user_id    INTEGER NOT NULL,
  contact_id INTEGER NOT NULL,
  batch      INTEGER NOT NULL,
  ts         INTEGER NOT NULL,
  PRIMARY KEY(user_id, contact_id)
) WITHOUT ROWID;
CREATE INDEX unlocked_batch ON unlocked(user_id, batch);
CREATE INDEX unlocked_contact ON unlocked(contact_id);
CREATE TABLE user_daily(
  user_id    INTEGER NOT NULL,
  day        INTEGER NOT NULL,
  ads        INTEGER NOT NULL DEFAULT 0,
  last_claim INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(user_id, day)
) WITHOUT ROWID;

-- Platform
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL, updated INTEGER NOT NULL, updated_by TEXT NOT NULL DEFAULT '') WITHOUT ROWID;
CREATE TABLE blocklist(domain TEXT PRIMARY KEY, source TEXT NOT NULL DEFAULT 'admin') WITHOUT ROWID;
CREATE TABLE verdicts(email TEXT PRIMARY KEY, ok INTEGER NOT NULL, reason TEXT NOT NULL, ts INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE mx_cache(domain TEXT PRIMARY KEY, ok INTEGER NOT NULL, reason TEXT NOT NULL, ts INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE suppress(k TEXT PRIMARY KEY, ts INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE removals(
  id     INTEGER PRIMARY KEY,
  kind   TEXT NOT NULL,             -- email | linkedin | phone | other
  value  TEXT NOT NULL,
  note   TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending', -- pending | done | rejected
  ts     INTEGER NOT NULL,
  done_ts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE uploads(
  id           INTEGER PRIMARY KEY,
  ts           INTEGER NOT NULL,
  filename     TEXT NOT NULL,
  bytes        INTEGER NOT NULL DEFAULT 0,
  rows         INTEGER NOT NULL DEFAULT 0,
  added        INTEGER NOT NULL DEFAULT 0,
  updated      INTEGER NOT NULL DEFAULT 0,
  unchanged    INTEGER NOT NULL DEFAULT 0,
  skipped      INTEGER NOT NULL DEFAULT 0,
  skip_reasons TEXT NOT NULL DEFAULT '{}',
  status       TEXT NOT NULL DEFAULT 'running', -- running | done | failed | cancelled
  error        TEXT NOT NULL DEFAULT '',
  notified     INTEGER NOT NULL DEFAULT 0,
  by           TEXT NOT NULL DEFAULT '',
  finished     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE campaigns(
  id        INTEGER PRIMARY KEY,
  upload_id INTEGER NOT NULL DEFAULT 0,
  subject   TEXT NOT NULL,
  intro     TEXT NOT NULL,
  added     INTEGER NOT NULL DEFAULT 0,
  created   INTEGER NOT NULL,
  total     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE outbox(
  id          INTEGER PRIMARY KEY,
  campaign_id INTEGER NOT NULL,
  user_id     INTEGER NOT NULL,
  status      TEXT NOT NULL DEFAULT 'queued', -- queued | sent | failed
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_at     INTEGER NOT NULL,
  sent_at     INTEGER NOT NULL DEFAULT 0,
  err         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_queue ON outbox(status, next_at);
CREATE UNIQUE INDEX outbox_once ON outbox(campaign_id, user_id);
CREATE TABLE mail_quota(day INTEGER NOT NULL, provider TEXT NOT NULL, sent INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(day, provider)) WITHOUT ROWID;

CREATE TABLE admins(
  id              INTEGER PRIMARY KEY,
  username        TEXT NOT NULL UNIQUE,
  pw_hash         TEXT NOT NULL,
  totp_secret     TEXT NOT NULL,
  totp_last_step  INTEGER NOT NULL DEFAULT 0,
  failed          INTEGER NOT NULL DEFAULT 0,
  locked_until    INTEGER NOT NULL DEFAULT 0,
  created         INTEGER NOT NULL
);
CREATE TABLE admin_sessions(
  id       TEXT PRIMARY KEY,
  admin_id INTEGER NOT NULL,
  created  INTEGER NOT NULL,
  exp      INTEGER NOT NULL,
  idle_exp INTEGER NOT NULL,
  csrf     TEXT NOT NULL,
  ip       TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE audit(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, actor TEXT NOT NULL, ip TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '');
CREATE INDEX audit_ts ON audit(ts);

CREATE TABLE seo_pages(
  slug     TEXT PRIMARY KEY,         -- e.g. country/united-states  industry/software  country/united-states/software
  kind     TEXT NOT NULL,            -- country | industry | combo
  country  INTEGER NOT NULL DEFAULT 0,
  industry INTEGER NOT NULL DEFAULT 0,
  cnt      INTEGER NOT NULL,
  updated  INTEGER NOT NULL,
  gone     INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE job_runs(name TEXT PRIMARY KEY, started INTEGER NOT NULL, finished INTEGER NOT NULL DEFAULT 0, ok INTEGER NOT NULL DEFAULT 0, msg TEXT NOT NULL DEFAULT '') WITHOUT ROWID;

INSERT INTO meta(k, v) VALUES ('dataset_version', '1'), ('created', CAST(strftime('%s','now') AS TEXT));
