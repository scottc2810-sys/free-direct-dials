-- Postgres contacts store (e.g. one Supabase free project). Everything lives in
-- schema fdd, which Supabase's REST/GraphQL APIs don't expose; row-level security
-- is on with no policies, so even an exposed schema would return no rows to the
-- anon/authenticated roles. Only the connection the app uses (the owner) reads it.
--
-- Same layout as the SQLite store: a narrow integer table for every filter,
-- count and facet, and a wide table read only for displayed rows. Keyword and
-- technology search use tsvector columns built from the app's own tokenizer.

CREATE TABLE IF NOT EXISTS fdd.dims(
  id      integer  PRIMARY KEY,
  dim     smallint NOT NULL,
  value_n text     NOT NULL,
  label   text     NOT NULL,
  cnt     bigint   NOT NULL DEFAULT 0,
  UNIQUE (dim, value_n)
);
CREATE SEQUENCE IF NOT EXISTS fdd.dims_seq START 13 OWNED BY fdd.dims.id;
ALTER TABLE fdd.dims ALTER COLUMN id SET DEFAULT nextval('fdd.dims_seq');
CREATE INDEX IF NOT EXISTS dims_cnt ON fdd.dims(dim, cnt DESC);
INSERT INTO fdd.dims(id, dim, value_n, label)
  SELECT g, g, '__blank__', '(Blank)' FROM generate_series(1, 12) AS g
  ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS fdd.dim_spell(
  dim_id integer NOT NULL,
  raw    text    NOT NULL,
  cnt    bigint  NOT NULL,
  PRIMARY KEY (dim_id, raw)
);

CREATE SEQUENCE IF NOT EXISTS fdd.contact_seq;
CREATE TABLE IF NOT EXISTS fdd.contacts(
  id        bigint  PRIMARY KEY DEFAULT nextval('fdd.contact_seq'),
  country   integer NOT NULL, state    integer NOT NULL, city     integer NOT NULL,
  industry  integer NOT NULL, sector   integer NOT NULL, ctype    integer NOT NULL,
  seniority integer NOT NULL, division integer NOT NULL, decision integer NOT NULL,
  csize     integer NOT NULL, revenue  integer NOT NULL, econf    integer NOT NULL,
  has_email boolean NOT NULL DEFAULT false,
  has_phone boolean NOT NULL DEFAULT false
);
ALTER SEQUENCE fdd.contact_seq OWNED BY fdd.contacts.id;
-- The narrow table is small enough (≈ 90 bytes a row) to scan for rare filter
-- combinations; indexes cover the dimensions visitors start from. Each index
-- costs space on a 500 MB free project, so there are only four.
CREATE INDEX IF NOT EXISTS c_country  ON fdd.contacts(country, industry);
CREATE INDEX IF NOT EXISTS c_industry ON fdd.contacts(industry, country);
CREATE INDEX IF NOT EXISTS c_state    ON fdd.contacts(state);
CREATE INDEX IF NOT EXISTS c_city     ON fdd.contacts(city);

CREATE TABLE IF NOT EXISTS fdd.contact_info(
  id        bigint PRIMARY KEY REFERENCES fdd.contacts(id) ON DELETE CASCADE,
  email_key text, li_key text, h_key text,
  name text NOT NULL DEFAULT '', title text NOT NULL DEFAULT '', company text NOT NULL DEFAULT '',
  website text NOT NULL DEFAULT '', linkedin text NOT NULL DEFAULT '',
  email text NOT NULL DEFAULT '', phone text NOT NULL DEFAULT '',
  extra   bytea,
  updated bigint NOT NULL,
  kw      tsvector,
  tech    tsvector,
  -- deferred: a batch may move a key from one contact to another
  CONSTRAINT ci_email_key UNIQUE (email_key) DEFERRABLE INITIALLY DEFERRED,
  CONSTRAINT ci_li_key    UNIQUE (li_key)    DEFERRABLE INITIALLY DEFERRED,
  CONSTRAINT ci_h_key     UNIQUE (h_key)     DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX IF NOT EXISTS ci_kw   ON fdd.contact_info USING gin (kw);
CREATE INDEX IF NOT EXISTS ci_tech ON fdd.contact_info USING gin (tech);

CREATE TABLE IF NOT EXISTS fdd.unlocked(
  user_id    bigint NOT NULL,
  contact_id bigint NOT NULL REFERENCES fdd.contacts(id) ON DELETE CASCADE,
  batch      bigint NOT NULL,
  ts         bigint NOT NULL,
  PRIMARY KEY (user_id, contact_id)
);
CREATE INDEX IF NOT EXISTS unlocked_batch   ON fdd.unlocked(user_id, batch DESC, contact_id);
CREATE INDEX IF NOT EXISTS unlocked_contact ON fdd.unlocked(contact_id);

ALTER TABLE fdd.meta         ENABLE ROW LEVEL SECURITY;
ALTER TABLE fdd.dims         ENABLE ROW LEVEL SECURITY;
ALTER TABLE fdd.dim_spell    ENABLE ROW LEVEL SECURITY;
ALTER TABLE fdd.contacts     ENABLE ROW LEVEL SECURITY;
ALTER TABLE fdd.contact_info ENABLE ROW LEVEL SECURITY;
ALTER TABLE fdd.unlocked     ENABLE ROW LEVEL SECURITY;
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    EXECUTE 'REVOKE ALL ON SCHEMA fdd FROM anon';
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    EXECUTE 'REVOKE ALL ON SCHEMA fdd FROM authenticated';
  END IF;
END $$;

INSERT INTO fdd.meta(k, v) VALUES
  ('dataset_version', '1'),
  ('stats', '{}'),
  ('store_uuid', md5(random()::text || clock_timestamp()::text))
ON CONFLICT DO NOTHING;
