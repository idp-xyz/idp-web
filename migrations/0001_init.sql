-- idp-web schema v1 (ADR-0001 D28, D29).
-- Run by the schema owner (`idp-web migrate`). The service itself connects as a login role that is a member of
-- idp_web_app: not a superuser, no BYPASSRLS, and with the four table privileges below and nothing else.
--
-- What is here: how many searches and page reads a tenant has made on a UTC day, and which number [n] each address has
-- for a caller. What is NOT here, on purpose: the query, the address, the page, the caller's subject (platform ADR-0003
-- appendix D 4.4). The caller and the address are stored as SHA-256 digests.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'idp_web_app') THEN
    CREATE ROLE idp_web_app NOLOGIN NOBYPASSRLS;
  END IF;
END $$;

-- the daily quota: one row per (tenant, UTC day, kind); the take is one conditional upsert (internal/state)
CREATE TABLE daily_usage (
  tenant text    NOT NULL CHECK (tenant <> ''),
  day    date    NOT NULL,
  kind   text    NOT NULL CHECK (kind IN ('searches', 'reads')),
  used   integer NOT NULL CHECK (used >= 0),
  PRIMARY KEY (tenant, day, kind)
);
CREATE INDEX daily_usage_day ON daily_usage (day);   -- the sweep deletes by day

-- source numbering (ADR-0001 D19): a caller's numbering lives from its first call until an hour without use
CREATE TABLE source_callers (
  caller    bytea       PRIMARY KEY CHECK (length(caller) = 32),   -- SHA-256 of tenant and subject, as the audit line hashes it
  next      integer     NOT NULL CHECK (next >= 1),                 -- the next free number; a number is never given twice
  last_used timestamptz NOT NULL
);
CREATE INDEX source_callers_last_used ON source_callers (last_used);   -- the sweep deletes by idle time

CREATE TABLE source_numbers (
  caller    bytea       NOT NULL REFERENCES source_callers (caller) ON DELETE CASCADE,
  address   bytea       NOT NULL CHECK (length(address) = 32),      -- SHA-256 of the canonical address, never the address
  n         integer     NOT NULL CHECK (n >= 1),
  last_used timestamptz NOT NULL,
  PRIMARY KEY (caller, address)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON daily_usage, source_callers, source_numbers TO idp_web_app;   -- DELETE: Release and the sweep
