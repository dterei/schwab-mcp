-- The schwabtx store, schema 1.
--
-- Every table is STRICT. INSTANT(c) and DAY(c) are expanded by history/schema.go into:
--   INSTANT(c): c GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]T[0-2][0-9]:[0-5][0-9]:[0-6][0-9].[0-9][0-9][0-9]Z'
--   DAY(c):     c GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'
-- An instant is UTC text of fixed width (text order is time order); a day is a New York
-- calendar date. NULL passes a CHECK. *_cents are integer cents, *_micros integer millionths.
-- Tables are evidence (never deleted, fixed by triggers but for the columns that only move
-- forward) or derived (a function of the evidence and the decisions under store.derived_with,
-- emptied and refilled at will). No derived column shares a row with raw JSON or file bytes:
-- derived values of an evidence row live in a table of their own keyed by its id (txn,
-- revision_cut, snapshot_fields, file_fields), so a derivation never rewrites evidence pages.

-- ---------------------------------------------------------------- the store

CREATE TABLE store (
  id               INTEGER PRIMARY KEY CHECK (id = 1),
  created_at       TEXT NOT NULL CHECK (INSTANT(created_at)),
  created_by       TEXT NOT NULL,                                 -- the schwabtx build that made it
  derived_with     INTEGER NOT NULL CHECK (derived_with > 0),     -- history.DeriveVersion
  calendar_version INTEGER NOT NULL CHECK (calendar_version > 0)  -- nyse.Version
) STRICT;

-- ---------------------------------------------------------------- accounts (evidence, two derived columns)

CREATE TABLE account (
  account_id       INTEGER PRIMARY KEY,
  number           TEXT NOT NULL UNIQUE CHECK (number <> '' AND number = trim(number)),  -- private: no view reads it
  masked           TEXT NOT NULL GENERATED ALWAYS AS
                     (CASE WHEN length(number) > 3 THEN '...' || substr(number, -3) ELSE '...' END) VIRTUAL,
  created_from     TEXT NOT NULL CHECK (created_from IN ('api','snapshot','file','user')),
  first_seen_at    TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  last_seen_at     TEXT NOT NULL CHECK (INSTANT(last_seen_at) AND last_seen_at >= first_seen_at),
  not_shared_since TEXT CHECK (INSTANT(not_shared_since)),   -- set by the sync (R 241-247)
  -- From GET /userPreference, latest read; nickname stored through mask.Text.
  nickname         TEXT,
  display_id       TEXT,
  primary_account  INTEGER CHECK (primary_account IN (0, 1)),
  lot_method       TEXT,
  -- Derived (small, nullable, no raw beside them): from the latest snapshot; from your
  -- 'account' decision.
  account_type     TEXT,
  kind             TEXT
) STRICT;

CREATE TABLE account_hash (
  account_id    INTEGER NOT NULL REFERENCES account,
  hash          TEXT NOT NULL CHECK (hash <> ''),
  first_seen_at TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  last_seen_at  TEXT NOT NULL CHECK (INSTANT(last_seen_at) AND last_seen_at >= first_seen_at),
  PRIMARY KEY (account_id, hash)
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- reads (evidence)

CREATE TABLE api_read (
  read_id        INTEGER PRIMARY KEY,
  endpoint       TEXT NOT NULL CHECK (endpoint IN
                   ('account_numbers','user_preference','accounts','transactions','transaction','file')),
  account_id     INTEGER REFERENCES account,
  file_id        INTEGER REFERENCES import_file,     -- endpoint 'file': rows from `schwabtx transactions -json`
  window_start   TEXT CHECK (INSTANT(window_start)), -- as sent, milliseconds kept
  window_end     TEXT CHECK (INSTANT(window_end)),
  types          TEXT,                               -- as sent, comma-separated
  all_types      INTEGER NOT NULL DEFAULT 0 CHECK (all_types IN (0, 1)),
  symbol         TEXT,
  activity_id    INTEGER,
  sent_at        TEXT NOT NULL CHECK (INSTANT(sent_at)),
  answered_at    TEXT NOT NULL CHECK (INSTANT(answered_at) AND answered_at >= sent_at),
  outcome        TEXT NOT NULL CHECK (outcome IN ('answered','refused','failed')),
  http_status    INTEGER,
  correlation_id TEXT,
  error          TEXT,                               -- through mask.Text
  row_count      INTEGER NOT NULL DEFAULT 0 CHECK (row_count >= 0),
  duplicates     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(duplicates)),  -- activity IDs sent twice
  capped         INTEGER NOT NULL DEFAULT 0 CHECK (capped IN (0, 1)),       -- row_count >= schwab.MaxTransactions
  complete       INTEGER NOT NULL DEFAULT 0 CHECK (complete IN (0, 1)),
  CHECK (endpoint NOT IN ('transactions','transaction','file') OR account_id IS NOT NULL),
  CHECK ((endpoint = 'file') = (file_id IS NOT NULL)),
  CHECK (endpoint <> 'transactions' OR (window_start IS NOT NULL AND window_end > window_start)),
  CHECK (endpoint <> 'transaction' OR activity_id IS NOT NULL),
  -- The roadmap's complete re-read: every type, no symbol, answered, under the cap.
  CHECK (complete = (endpoint = 'transactions' AND outcome = 'answered' AND all_types = 1
                     AND symbol IS NULL AND capped = 0))
) STRICT;
CREATE INDEX api_read_account ON api_read (account_id, endpoint, read_id);

-- ---------------------------------------------------------------- API transactions

CREATE TABLE api_txn (                         -- evidence only
  txn_id          INTEGER PRIMARY KEY,
  account_id      INTEGER NOT NULL REFERENCES account,    -- the account read, not the row's accountNumber
  activity_id     INTEGER CHECK (activity_id > 0),       -- NULL: Schwab sent none (flagged)
  id_source       TEXT NOT NULL CHECK (id_source IN ('number','string','none')),
  first_raw       TEXT NOT NULL,                          -- the first raw JSON seen
  first_read_id   INTEGER NOT NULL REFERENCES api_read,
  first_seen_at   TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  latest_raw      TEXT NOT NULL,                          -- the latest raw JSON an API read returned
  latest_read_id  INTEGER NOT NULL REFERENCES api_read,
  latest_seen_at  TEXT NOT NULL CHECK (INSTANT(latest_seen_at) AND latest_seen_at >= first_seen_at),
  latest_list_read_id INTEGER REFERENCES api_read,        -- the last 'transactions' read that returned it (9.5)
  CHECK ((activity_id IS NULL) = (id_source = 'none')),
  UNIQUE (account_id, activity_id)                        -- NULLs are distinct
) STRICT;

CREATE TABLE txn (                             -- derived: an API transaction's columns
  txn_id          INTEGER PRIMARY KEY REFERENCES api_txn,
  account_id      INTEGER NOT NULL REFERENCES account,    -- copies, for the indexes
  activity_id     INTEGER,
  -- From latest_raw (layer 1).
  time            TEXT CHECK (INSTANT(time)),
  trade_date      TEXT CHECK (INSTANT(trade_date)),
  trade_day       TEXT CHECK (DAY(trade_day)),           -- Transaction.TradeDay
  settlement_date TEXT CHECK (INSTANT(settlement_date)),
  settlement_day  TEXT CHECK (DAY(settlement_day)),
  type            TEXT,
  status          TEXT,
  sub_account     TEXT,
  description     TEXT,                                   -- through mask.Text (free text)
  net_cents       INTEGER,                                -- NULL: absent or off the cent grid (flagged)
  order_id        INTEGER,
  position_id     INTEGER,
  activity_type   TEXT,
  qualified_dividend INTEGER CHECK (qualified_dividend IN (0, 1)),
  number_sent_masked TEXT,                                -- the row's accountNumber, masked, when it differs
  problems        TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(problems)),
  -- From the revisions and the reads (layer 2).
  revisions       INTEGER NOT NULL DEFAULT 1 CHECK (revisions >= 1),
  gone_read_id    INTEGER REFERENCES api_read             -- the complete read that shows it gone (9.5)
) STRICT;
CREATE INDEX txn_time ON txn (account_id, time, activity_id);
CREATE INDEX txn_day ON txn (account_id, trade_day);
CREATE INDEX txn_order ON txn (account_id, order_id) WHERE order_id IS NOT NULL;

CREATE TABLE api_revision (                    -- evidence only
  revision_id   INTEGER PRIMARY KEY,
  txn_id        INTEGER NOT NULL REFERENCES api_txn,
  seq           INTEGER NOT NULL CHECK (seq >= 1),      -- runs of one cut form, in the order of the reads
  raw           TEXT NOT NULL,                          -- the first raw JSON of the run
  first_read_id INTEGER NOT NULL REFERENCES api_read,
  first_seen_at TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  last_read_id  INTEGER NOT NULL REFERENCES api_read,
  last_seen_at  TEXT NOT NULL CHECK (INSTANT(last_seen_at) AND last_seen_at >= first_seen_at),
  UNIQUE (txn_id, seq)
) STRICT;

CREATE TABLE revision_cut (                    -- derived from api_revision.raw
  revision_id INTEGER PRIMARY KEY REFERENCES api_revision,
  cut_version INTEGER NOT NULL CHECK (cut_version >= 1),
  digest      BLOB NOT NULL CHECK (length(digest) = 32),   -- SHA-256 of the cut form
  changed     TEXT CHECK (changed IS NULL OR json_valid(changed))  -- key paths that differ from seq-1
) STRICT;

CREATE TABLE api_absent (     -- evidence: a complete read that did not return a stored transaction near its window
  read_id INTEGER NOT NULL REFERENCES api_read,
  txn_id  INTEGER NOT NULL REFERENCES api_txn,
  PRIMARY KEY (read_id, txn_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX api_absent_txn ON api_absent (txn_id, read_id);

CREATE TABLE api_leg (                                  -- derived from api_txn.latest_raw
  txn_id          INTEGER NOT NULL REFERENCES api_txn,
  leg             INTEGER NOT NULL CHECK (leg >= 0),    -- index in transferItems
  asset_type      TEXT,
  instrument_id   INTEGER,
  symbol          TEXT,                                 -- as sent: reference data as of the read
  security_id     INTEGER REFERENCES security,
  amount_micros   INTEGER,     -- shares or contracts on a security leg; the fee or cash on a CURRENCY leg
  amount_cents    INTEGER,     -- a CURRENCY leg's amount in cents (the `legs` view's cash_cents)
  cost_cents      INTEGER,
  price           REAL,
  fee_type        TEXT,
  position_effect TEXT,
  PRIMARY KEY (txn_id, leg),
  CHECK (amount_cents IS NULL OR asset_type = 'CURRENCY')
) STRICT, WITHOUT ROWID;
CREATE INDEX api_leg_security ON api_leg (security_id) WHERE security_id IS NOT NULL;

-- ---------------------------------------------------------------- securities (derived)

CREATE TABLE security (
  security_id       INTEGER PRIMARY KEY,
  canonical_key     TEXT NOT NULL UNIQUE,     -- the least of its keys: i: before c: before x: before s:
  definite          INTEGER NOT NULL CHECK (definite IN (0, 1)),  -- holds an i:, c: or x: key
  asset_type        TEXT,
  type              TEXT,
  put_call          TEXT CHECK (put_call IN ('PUT','CALL')),
  strike            REAL,
  expiration_day    TEXT CHECK (DAY(expiration_day)),
  multiplier        REAL,
  underlying_symbol TEXT,
  underlying_id     INTEGER REFERENCES security,
  first_seen_at     TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  last_seen_at      TEXT NOT NULL CHECK (INSTANT(last_seen_at) AND last_seen_at >= first_seen_at)
) STRICT;

CREATE TABLE instrument_key (
  -- i:<instrumentId>  c:<cusip>  x:<ItemIssueId>  s:<assetType>:<symbol>  s:FILE:<symbol>@<export date>
  key               TEXT PRIMARY KEY CHECK (key GLOB '[icxs]:?*'),
  security_id       INTEGER NOT NULL REFERENCES security,
  asset_type        TEXT,
  type              TEXT,
  put_call          TEXT,
  strike            REAL,
  expiration_day    TEXT CHECK (DAY(expiration_day)),
  multiplier        REAL,
  underlying_symbol TEXT,
  first_seen_at     TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  last_seen_at      TEXT NOT NULL CHECK (INSTANT(last_seen_at) AND last_seen_at >= first_seen_at)
) STRICT, WITHOUT ROWID;
CREATE INDEX instrument_key_security ON instrument_key (security_id);

CREATE TABLE key_name (
  key           TEXT NOT NULL REFERENCES instrument_key,
  kind          TEXT NOT NULL CHECK (kind IN ('symbol','uniform_symbol','cusip','description')),
  value         TEXT NOT NULL CHECK (value <> ''),   -- a 'description' through mask.Text
  seen_in       TEXT NOT NULL CHECK (seen_in IN ('transaction','deliverable','position','history_file','realized_file')),
  first_seen_at TEXT NOT NULL CHECK (INSTANT(first_seen_at)),
  last_seen_at  TEXT NOT NULL CHECK (INSTANT(last_seen_at) AND last_seen_at >= first_seen_at),
  PRIMARY KEY (key, kind, value)
) STRICT, WITHOUT ROWID;
CREATE INDEX key_name_value ON key_name (kind, value, first_seen_at);

-- ---------------------------------------------------------------- snapshots

CREATE TABLE snapshot_import (                 -- evidence: one import of a snapshot file
  import_id   INTEGER PRIMARY KEY,
  label       TEXT NOT NULL,                   -- the file's base name, through mask.Text
  imported_at TEXT NOT NULL CHECK (INSTANT(imported_at)),
  lines       INTEGER NOT NULL CHECK (lines >= 0),
  new         INTEGER NOT NULL CHECK (new >= 0),
  known       INTEGER NOT NULL CHECK (known >= 0),
  skipped     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(skipped))   -- reason -> count
) STRICT;

CREATE TABLE snapshot (                         -- evidence only
  snapshot_id      INTEGER PRIMARY KEY,
  fetched_at       TEXT NOT NULL CHECK (INSTANT(fetched_at)),
  raw              TEXT NOT NULL,                -- the element as in the file (or Schwab's bytes), json.Compact
  digest           BLOB NOT NULL CHECK (length(digest) = 32),   -- SHA-256 of raw
  source           TEXT NOT NULL CHECK (source IN ('file','sync')),
  import_id        INTEGER REFERENCES snapshot_import,
  line             INTEGER CHECK (line >= 1),
  line_version     INTEGER,
  read_id          INTEGER REFERENCES api_read,
  CHECK ((source = 'file') = (import_id IS NOT NULL AND line IS NOT NULL)),
  CHECK ((source = 'sync') = (read_id IS NOT NULL)),
  UNIQUE (fetched_at, digest)
) STRICT;

CREATE TABLE snapshot_fields (                  -- derived from snapshot.raw, and the market dates from nyse
  snapshot_id      INTEGER PRIMARY KEY REFERENCES snapshot,
  fetched_at       TEXT NOT NULL CHECK (INSTANT(fetched_at)),   -- a copy, for the index
  account_id       INTEGER REFERENCES account,   -- NULL: its number could not be read (flagged)
  market_date      TEXT CHECK (DAY(market_date)),        -- the last session closed at or before fetched_at
  in_session_date  TEXT CHECK (DAY(in_session_date)),    -- the session under way at fetched_at, if any
  calendar_version INTEGER,
  account_type     TEXT,
  round_trips      INTEGER,
  is_day_trader    INTEGER CHECK (is_day_trader IN (0, 1)),
  closing_only     INTEGER CHECK (closing_only IN (0, 1)),
  pfcb             INTEGER CHECK (pfcb IN (0, 1)),
  portfolio_margin INTEGER CHECK (portfolio_margin IN (0, 1)),
  intraday_margin  INTEGER CHECK (intraday_margin IN (0, 1)),
  initial_liquidation_cents    INTEGER,
  current_liquidation_cents    INTEGER,
  aggregated_liquidation_cents INTEGER,
  cash_cents       INTEGER,                      -- currentBalances.cashBalance
  balances         TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(balances)),
                   -- {"initial"|"current"|"projected"|"aggregated": {"<figure>": {"v": float, "c": cents|null}}}
  problems         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(problems))
) STRICT;
CREATE INDEX snapshot_fields_account ON snapshot_fields (account_id, market_date, fetched_at);

CREATE TABLE snapshot_position (               -- derived at insert; a figure Schwab did not send is NULL
  snapshot_id          INTEGER NOT NULL REFERENCES snapshot,
  position             INTEGER NOT NULL CHECK (position >= 0),
  security_id          INTEGER REFERENCES security,
  asset_type           TEXT,
  symbol               TEXT,
  cusip                TEXT,
  description          TEXT,                     -- through mask.Text (free text)
  long_micros          INTEGER,
  short_micros         INTEGER,
  settled_long_micros  INTEGER,
  settled_short_micros INTEGER,
  prev_long_micros     INTEGER,
  prev_short_micros    INTEGER,
  aged_micros          INTEGER,
  average_price        REAL,
  average_long_price   REAL,
  average_short_price  REAL,
  taxlot_long_price    REAL,
  taxlot_short_price   REAL,
  market_value         REAL,
  maintenance          REAL,
  long_open_pl         REAL,
  short_open_pl        REAL,
  day_pl               REAL,
  day_pl_pct           REAL,
  day_cost             REAL,
  problems             TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(problems)),
  PRIMARY KEY (snapshot_id, position)
) STRICT, WITHOUT ROWID;
CREATE INDEX snapshot_position_security ON snapshot_position (security_id) WHERE security_id IS NOT NULL;

CREATE TABLE market_session (                  -- derived from internal/nyse, over the snapshots' span
  date     TEXT PRIMARY KEY CHECK (DAY(date)),
  open_at  TEXT NOT NULL CHECK (INSTANT(open_at)),
  close_at TEXT NOT NULL CHECK (INSTANT(close_at) AND close_at > open_at),
  early    INTEGER NOT NULL CHECK (early IN (0, 1)),
  verified INTEGER NOT NULL CHECK (verified IN (0, 1))    -- year <= nyse.VerifiedThrough
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- files from schwab.com

CREATE TABLE import_file (                      -- evidence only
  file_id            INTEGER PRIMARY KEY,
  account_id         INTEGER REFERENCES account,  -- the account named at import; NULL only for kind 'api_json'
  kind               TEXT NOT NULL CHECK (kind IN ('history_csv','history_json','realized_csv','api_json')),
  name               TEXT NOT NULL,               -- the base name as given: private
  content            BLOB NOT NULL,               -- the file's bytes
  sha256             BLOB NOT NULL UNIQUE CHECK (length(sha256) = 32),
  imported_at        TEXT NOT NULL CHECK (INSTANT(imported_at)),
  user_from          TEXT CHECK (DAY(user_from)), -- the coverage you gave at import
  user_to            TEXT CHECK (DAY(user_to) AND user_to >= user_from),
  CHECK ((kind = 'api_json') = (account_id IS NULL))
) STRICT;

CREATE TABLE file_fields (                      -- derived from the content, the decisions and the other sources
  file_id            INTEGER PRIMARY KEY REFERENCES import_file,
  ref                TEXT NOT NULL UNIQUE,        -- F + the shortest prefix of 8 or more hex digits unique among files
  account_id         INTEGER REFERENCES account,  -- in effect: your file_account decision, else import_file's
  forgotten          INTEGER NOT NULL DEFAULT 0 CHECK (forgotten IN (0, 1)),   -- your forget decision
  label              TEXT,                        -- 14.6: the name with only its free text masked
  layout             TEXT,
  encoding           TEXT CHECK (encoding IN ('utf-8','windows-1252')),
  exported_at        TEXT CHECK (INSTANT(exported_at)),
  stated_from        TEXT CHECK (DAY(stated_from)),
  stated_to          TEXT CHECK (DAY(stated_to)),
  rows_from          TEXT CHECK (DAY(rows_from)),
  rows_to            TEXT CHECK (DAY(rows_to)),
  covers_from        TEXT CHECK (DAY(covers_from)),
  covers_to          TEXT CHECK (DAY(covers_to) AND covers_to >= covers_from),
  coverage_source    TEXT CHECK (coverage_source IN ('stated','user','rows','api_rows')),
  completeness       TEXT CHECK (completeness IN ('complete','filtered','unknown')),
  complete_by        TEXT CHECK (complete_by IN ('you','api','file')),   -- what established 'complete'
  lacks_rows         INTEGER CHECK (lacks_rows >= 0),   -- filter-shaped lacks (14.6)
  row_count          INTEGER CHECK (row_count >= 0),
  stated_total_cents INTEGER,
  rows_total_cents   INTEGER
) STRICT;
CREATE INDEX file_fields_account ON file_fields (account_id);

CREATE TABLE history_row (                       -- derived from import_file.content at import
  row_id          INTEGER PRIMARY KEY,
  file_id         INTEGER NOT NULL REFERENCES import_file,
  ordinal         INTEGER NOT NULL CHECK (ordinal >= 1),   -- order among the file's rows
  line            INTEGER NOT NULL CHECK (line >= 1),      -- where it starts in the file
  raw             TEXT NOT NULL,                           -- the row as the file has it, as UTF-8
  posted_date     TEXT CHECK (DAY(posted_date)),
  asof_date       TEXT CHECK (DAY(asof_date)),
  action          TEXT NOT NULL,
  symbol          TEXT NOT NULL,
  description     TEXT NOT NULL,                           -- through mask.Text
  quantity_micros INTEGER,                                 -- signed by the action (13.1)
  quantity_text   TEXT NOT NULL,                           -- canonical exact decimal, '' when blank
  price           REAL,
  price_text      TEXT NOT NULL,
  fees_cents      INTEGER,
  amount_cents    INTEGER,
  item_issue_id   TEXT,
  acctg_rule_cd   TEXT,
  coarse          BLOB NOT NULL CHECK (length(coarse) = 32),   -- the coarse identity (14.5)
  problems        TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(problems)),
  UNIQUE (file_id, ordinal)
) STRICT;
CREATE INDEX history_row_coarse ON history_row (coarse);

CREATE TABLE csv_txn (                           -- derived per account: each transaction once across files
  csv_txn_id      INTEGER PRIMARY KEY,
  account_id      INTEGER NOT NULL REFERENCES account,
  ref             TEXT NOT NULL,                 -- C<hex>.<ordinal> of the row that opened it (5.4)
  subject         TEXT NOT NULL,                 -- c:<sha256 hex>:<ordinal>
  coarse          BLOB NOT NULL CHECK (length(coarse) = 32),
  shown_row_id    INTEGER NOT NULL REFERENCES history_row,   -- its row in the newest export
  quantity_as_of  TEXT CHECK (DAY(quantity_as_of)),          -- that export's date (13.1)
  posted_date     TEXT NOT NULL CHECK (DAY(posted_date)),
  asof_date       TEXT CHECK (DAY(asof_date)),
  event_date      TEXT NOT NULL CHECK (DAY(event_date)),    -- asof_date, else posted_date
  files           INTEGER NOT NULL CHECK (files >= 1),      -- how many files hold it
  UNIQUE (account_id, ref),
  UNIQUE (subject)
) STRICT;

CREATE TABLE csv_member (                        -- derived: which rows make which file transaction
  row_id     INTEGER PRIMARY KEY REFERENCES history_row,
  csv_txn_id INTEGER NOT NULL REFERENCES csv_txn
) STRICT;
CREATE INDEX csv_member_txn ON csv_member (csv_txn_id);

CREATE TABLE realized_row (                      -- derived from import_file.content at import
  row_id     INTEGER PRIMARY KEY,
  file_id    INTEGER NOT NULL REFERENCES import_file,
  ordinal    INTEGER NOT NULL CHECK (ordinal >= 1),
  line       INTEGER NOT NULL CHECK (line >= 1),
  raw        TEXT NOT NULL,
  symbol     TEXT NOT NULL,
  name       TEXT,                                -- through mask.Text (free text)
  closed_date           TEXT CHECK (DAY(closed_date)),
  opened_date           TEXT CHECK (DAY(opened_date)),
  opened_text           TEXT NOT NULL,           -- Opened Date as written, through mask.Text (free text)
  quantity_micros       INTEGER,
  quantity_text         TEXT NOT NULL,
  proceeds_per_share    REAL,
  cost_per_share        REAL,
  proceeds_cents        INTEGER,
  cost_basis_cents      INTEGER,
  gain_cents            INTEGER,
  gain_pct              TEXT,
  lt_gain_cents         INTEGER,
  st_gain_cents         INTEGER,
  term                  TEXT,
  unadjusted_cost_cents INTEGER,
  wash_sale             INTEGER CHECK (wash_sale IN (0, 1)),
  disallowed_loss_cents INTEGER,
  txn_closed_date       TEXT CHECK (DAY(txn_closed_date)),
  txn_cost_basis_cents  INTEGER,
  txn_gain_cents        INTEGER,
  txn_gain_pct          TEXT,
  lt_txn_gain_cents     INTEGER,
  lt_txn_gain_pct       TEXT,
  st_txn_gain_cents     INTEGER,
  st_txn_gain_pct       TEXT,
  coarse     BLOB NOT NULL CHECK (length(coarse) = 32),   -- closed date, opened text, quantity, proceeds
  fine       BLOB NOT NULL CHECK (length(fine) = 32),     -- every column as written
  problems   TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(problems)),
  UNIQUE (file_id, ordinal)
) STRICT;

CREATE TABLE realized_lot (                      -- derived per account: each closed lot once across files
  lot_id         INTEGER PRIMARY KEY,
  account_id     INTEGER NOT NULL REFERENCES account,
  ref            TEXT NOT NULL,                  -- L<hex>.<ordinal> (5.4)
  subject        TEXT NOT NULL,                  -- l:<sha256 hex>:<ordinal>
  shown_row_id   INTEGER NOT NULL REFERENCES realized_row,   -- from the newest export
  versions       INTEGER NOT NULL CHECK (versions >= 1),     -- distinct fine digests among its rows
  files          INTEGER NOT NULL CHECK (files >= 1),
  security_id    INTEGER REFERENCES security,
  UNIQUE (account_id, ref),
  UNIQUE (subject)
) STRICT;

CREATE TABLE lot_member (
  row_id INTEGER PRIMARY KEY REFERENCES realized_row,
  lot_id INTEGER NOT NULL REFERENCES realized_lot
) STRICT;

-- ---------------------------------------------------------------- the ledger (derived per account)

CREATE TABLE entry (
  entry_id        INTEGER PRIMARY KEY,
  account_id      INTEGER NOT NULL REFERENCES account,
  txn_id          INTEGER UNIQUE REFERENCES api_txn,
  csv_txn_id      INTEGER UNIQUE REFERENCES csv_txn,
  ref             TEXT NOT NULL,          -- T<activityId> or C<hex>.<ordinal> (5.4)
  subject         TEXT NOT NULL,          -- the decision subject (5.4)
  kind            TEXT,                   -- Schwab's type, or the file's Action
  description     TEXT,                   -- through mask.Text (free text)
  day             TEXT CHECK (DAY(day)),  -- trade_day, or the file's event_date
  posted_date     TEXT CHECK (DAY(posted_date)),   -- file entries
  time            TEXT CHECK (INSTANT(time)),
  amount_cents    INTEGER,                -- netAmount, or the file's Amount (blank is 0)
  fees_cents      INTEGER,
  gross_cents     INTEGER,                -- amount + fees, for both sources (13.1)
  quantity_micros INTEGER,                -- of its one security leg, signed: + in, - out
  quantity_as_of  TEXT CHECK (DAY(quantity_as_of)),   -- file entries: the shown export's date
  price           REAL,
  symbol          TEXT,                   -- as the row names it: a leg, ~SYM, or the file's Symbol
  security_id     INTEGER REFERENCES security,
  family          TEXT NOT NULL,          -- matching family (15.1)
  category        TEXT NOT NULL CHECK (category IN ('deposit','withdrawal','transfer_in','transfer_out',
                    'journal','buy','sell','dividend','interest','fee','tax','other')),
  category_rule   TEXT NOT NULL,          -- the rule's name, or 'user'
  pair_kind       TEXT CHECK (pair_kind IN ('journal','sweep','reversal')),
  pair_entry_id   INTEGER REFERENCES entry,
  counted         INTEGER NOT NULL CHECK (counted IN (0, 1)),
  counted_why     TEXT NOT NULL CHECK (counted_why IN ('api','gone','invalid','user','linked','uncertain','held_out','policy','file','tda')),
  CHECK ((txn_id IS NULL) <> (csv_txn_id IS NULL)),
  CHECK ((pair_kind IS NULL) = (pair_entry_id IS NULL)),
  UNIQUE (account_id, ref)
) STRICT;
CREATE INDEX entry_day ON entry (account_id, day);
CREATE INDEX entry_category ON entry (category, account_id, day);
CREATE INDEX entry_security ON entry (security_id) WHERE security_id IS NOT NULL;

-- A match: API transactions and file transactions that are one and the same.
CREATE TABLE match (
  match_id   INTEGER PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES account,
  method     TEXT NOT NULL CHECK (method IN ('exact','unique','fills','legs','user')),
  score      REAL
) STRICT;

CREATE TABLE match_member (
  match_id   INTEGER NOT NULL REFERENCES match,
  txn_id     INTEGER UNIQUE REFERENCES api_txn,        -- in one match at most
  csv_txn_id INTEGER UNIQUE REFERENCES csv_txn,        -- in one match at most
  CHECK ((txn_id IS NULL) <> (csv_txn_id IS NULL))
) STRICT;
CREATE INDEX match_member_match ON match_member (match_id);

CREATE TABLE match_candidate (                  -- for an uncertain file transaction: what it may be
  csv_txn_id INTEGER NOT NULL REFERENCES csv_txn,
  txn_id     INTEGER NOT NULL REFERENCES api_txn,
  score      REAL NOT NULL,
  why        TEXT NOT NULL,
  PRIMARY KEY (csv_txn_id, txn_id)
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- your decisions (evidence)

CREATE TABLE decision (
  decision_id INTEGER PRIMARY KEY,
  made_at     TEXT NOT NULL CHECK (INSTANT(made_at)),
  kind        TEXT NOT NULL CHECK (kind IN ('category','match','reject','count','pair','completeness',
                'file_account','file_range','forget','action','security','entry_security','account')),
  subject     TEXT NOT NULL CHECK (subject <> ''),   -- a stable key (5.4), never a derived row's id
  value       TEXT NOT NULL,                         -- 'auto' withdraws the subject's earlier decisions of the kind
  basis       TEXT CHECK (basis IS NULL OR json_valid(basis)),  -- what the subject was when decided (13.7)
  note        TEXT                                   -- through mask.Text
) STRICT;
CREATE INDEX decision_subject ON decision (kind, subject, decision_id);

-- ---------------------------------------------------------------- flags and coverage (derived)

CREATE TABLE flag_def (                          -- rewritten from flags.go by every derivation
  code     TEXT PRIMARY KEY CHECK (code GLOB '[a-z]*' AND code NOT GLOB '*[^a-z_]*'),
  subject  TEXT NOT NULL CHECK (subject IN ('entry','snapshot','file','lot')),
  severity TEXT NOT NULL CHECK (severity IN ('attention','info')),
  meaning  TEXT NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE flag (
  flag_id     INTEGER PRIMARY KEY,
  entry_id    INTEGER REFERENCES entry,
  snapshot_id INTEGER REFERENCES snapshot,
  file_id     INTEGER REFERENCES import_file,
  lot_id      INTEGER REFERENCES realized_lot,
  code        TEXT NOT NULL REFERENCES flag_def,
  detail      TEXT,                                -- through mask.Text
  CHECK ((entry_id IS NOT NULL) + (snapshot_id IS NOT NULL) + (file_id IS NOT NULL) + (lot_id IS NOT NULL) = 1)
) STRICT;
CREATE UNIQUE INDEX flag_entry ON flag (entry_id, code) WHERE entry_id IS NOT NULL;
CREATE UNIQUE INDEX flag_snapshot ON flag (snapshot_id, code) WHERE snapshot_id IS NOT NULL;
CREATE UNIQUE INDEX flag_file ON flag (file_id, code) WHERE file_id IS NOT NULL;
CREATE UNIQUE INDEX flag_lot ON flag (lot_id, code) WHERE lot_id IS NOT NULL;
CREATE INDEX flag_code ON flag (code);

CREATE TABLE coverage (
  account_id INTEGER NOT NULL REFERENCES account,
  source     TEXT NOT NULL CHECK (source IN ('api','api_file','file','file_unknown','file_filtered','realized','snapshot')),
  from_date  TEXT NOT NULL CHECK (DAY(from_date)),
  to_date    TEXT NOT NULL CHECK (DAY(to_date) AND to_date >= from_date),
  PRIMARY KEY (account_id, source, from_date)
) STRICT, WITHOUT ROWID;

-- ---------------------------------------------------------------- sync (written by `schwabtx sync`, later)

CREATE TABLE sync_state (
  account_id          INTEGER PRIMARY KEY REFERENCES account,
  floor_date          TEXT CHECK (DAY(floor_date)),          -- the start date you set
  backfill            TEXT NOT NULL DEFAULT 'none'
                        CHECK (backfill IN ('none','walking','ended','floor','refused','interrupted','spent')),
  backfill_read_id    INTEGER REFERENCES api_read,           -- the read that ended or stopped it
  backfill_detail     TEXT,                                  -- through mask.Text
  oldest_seen_at      TEXT CHECK (INSTANT(oldest_seen_at)),
  empty_windows       INTEGER NOT NULL DEFAULT 0 CHECK (empty_windows >= 0),
  last_incremental_at TEXT CHECK (INSTANT(last_incremental_at)),
  overlap_days        INTEGER CHECK (overlap_days >= 0),
  updated_at          TEXT NOT NULL CHECK (INSTANT(updated_at))
) STRICT;

-- ---------------------------------------------------------------- what never changes (evidence only)

CREATE TRIGGER account_number_fixed BEFORE UPDATE OF number ON account
BEGIN SELECT RAISE(ABORT, 'store: an account''s number never changes'); END;
CREATE TRIGGER account_kept BEFORE DELETE ON account
BEGIN SELECT RAISE(ABORT, 'store: accounts are never deleted'); END;
CREATE TRIGGER account_hash_kept BEFORE DELETE ON account_hash
BEGIN SELECT RAISE(ABORT, 'store: hashes seen are never deleted'); END;
CREATE TRIGGER api_read_fixed BEFORE UPDATE ON api_read
BEGIN SELECT RAISE(ABORT, 'store: a read is recorded once'); END;
CREATE TRIGGER api_read_kept BEFORE DELETE ON api_read
BEGIN SELECT RAISE(ABORT, 'store: reads are never deleted'); END;
CREATE TRIGGER api_txn_first_fixed
  BEFORE UPDATE OF txn_id, account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at ON api_txn
BEGIN SELECT RAISE(ABORT, 'store: a transaction''s identity and first raw JSON never change'); END;
CREATE TRIGGER api_txn_latest_forward BEFORE UPDATE OF latest_read_id, latest_list_read_id ON api_txn
  WHEN NEW.latest_read_id < OLD.latest_read_id
    OR NEW.latest_list_read_id IS NULL AND OLD.latest_list_read_id IS NOT NULL
    OR NEW.latest_list_read_id < OLD.latest_list_read_id
BEGIN SELECT RAISE(ABORT, 'store: a transaction''s latest raw JSON only moves forward'); END;
CREATE TRIGGER api_txn_kept BEFORE DELETE ON api_txn
BEGIN SELECT RAISE(ABORT, 'store: transactions are never deleted'); END;
CREATE TRIGGER api_revision_fixed
  BEFORE UPDATE OF revision_id, txn_id, seq, raw, first_read_id, first_seen_at ON api_revision
BEGIN SELECT RAISE(ABORT, 'store: a revision''s raw JSON never changes'); END;
CREATE TRIGGER api_revision_kept BEFORE DELETE ON api_revision
BEGIN SELECT RAISE(ABORT, 'store: revisions are never deleted'); END;
CREATE TRIGGER api_absent_fixed BEFORE UPDATE ON api_absent
BEGIN SELECT RAISE(ABORT, 'store: what a read left out is recorded once'); END;
CREATE TRIGGER api_absent_kept BEFORE DELETE ON api_absent
BEGIN SELECT RAISE(ABORT, 'store: what a read left out is never deleted'); END;
CREATE TRIGGER snapshot_import_kept BEFORE DELETE ON snapshot_import
BEGIN SELECT RAISE(ABORT, 'store: snapshot imports are never deleted'); END;
CREATE TRIGGER snapshot_fixed BEFORE UPDATE ON snapshot
BEGIN SELECT RAISE(ABORT, 'store: a snapshot never changes'); END;
CREATE TRIGGER snapshot_kept BEFORE DELETE ON snapshot
BEGIN SELECT RAISE(ABORT, 'store: snapshots are never deleted'); END;
CREATE TRIGGER import_file_fixed BEFORE UPDATE ON import_file
BEGIN SELECT RAISE(ABORT, 'store: an imported file never changes; your decisions on it are in decision'); END;
CREATE TRIGGER import_file_kept BEFORE DELETE ON import_file
BEGIN SELECT RAISE(ABORT, 'store: imported files are never deleted; import -forget sets them aside'); END;
CREATE TRIGGER decision_fixed BEFORE UPDATE ON decision
BEGIN SELECT RAISE(ABORT, 'store: decisions are never changed, only followed by newer ones'); END;
CREATE TRIGGER decision_kept BEFORE DELETE ON decision
BEGIN SELECT RAISE(ABORT, 'store: decisions are never deleted'); END;

-- ---------------------------------------------------------------- views
--
-- What milestones 3 and 4 read and `schwabtx export` writes. No view reads account.number,
-- a raw column, import_file.name or import_file.content. Every view column is either free
-- text, stored through mask.Text (descriptions, nicknames, names, labels, flag details,
-- notes, errors), or an identifier, amount, date or code, listed as such in history's
-- viewColumns table (5.3), which TestViewsHoldNoAccountNumber and TestDocsListEverything
-- read. No view exposes a derived row's id: entries, lots and file transactions are named by
-- refs, securities by their canonical key.

CREATE VIEW accounts AS
SELECT a.account_id, 'A' || a.account_id AS ref, a.masked AS account, a.nickname, a.account_type, a.kind,
       a.primary_account, a.lot_method,
       (SELECT h.hash FROM account_hash h WHERE h.account_id = a.account_id
         ORDER BY h.last_seen_at DESC, h.hash LIMIT 1) AS hash,
       a.first_seen_at, a.last_seen_at, a.not_shared_since,
       (EXISTS (SELECT 1 FROM api_txn t WHERE t.account_id = a.account_id)
        OR EXISTS (SELECT 1 FROM snapshot_fields f WHERE f.account_id = a.account_id)
        OR EXISTS (SELECT 1 FROM file_fields f WHERE f.account_id = a.account_id AND f.forgotten = 0)
        OR EXISTS (SELECT 1 FROM account_hash h WHERE h.account_id = a.account_id)) AS used
FROM account a;

CREATE VIEW securities AS
SELECT s.canonical_key AS security_key, s.definite, s.asset_type, s.type,
       (SELECT n.value FROM key_name n JOIN instrument_key k ON k.key = n.key
         WHERE k.security_id = s.security_id AND n.kind = 'symbol'
         ORDER BY n.last_seen_at DESC, n.value LIMIT 1) AS symbol,
       (SELECT n.value FROM key_name n JOIN instrument_key k ON k.key = n.key
         WHERE k.security_id = s.security_id AND n.kind = 'cusip'
         ORDER BY n.last_seen_at DESC, n.value LIMIT 1) AS cusip,
       (SELECT n.value FROM key_name n JOIN instrument_key k ON k.key = n.key
         WHERE k.security_id = s.security_id AND n.kind = 'description'
         ORDER BY n.last_seen_at DESC, n.value LIMIT 1) AS description,
       s.put_call, s.strike, s.expiration_day, s.multiplier, s.underlying_symbol,
       (SELECT u.canonical_key FROM security u WHERE u.security_id = s.underlying_id) AS underlying_key,
       s.first_seen_at, s.last_seen_at
FROM security s;

CREATE VIEW security_names AS
SELECT s.canonical_key AS security_key, n.key, n.kind, n.value, n.seen_in, n.first_seen_at, n.last_seen_at
FROM key_name n JOIN instrument_key k ON k.key = n.key JOIN security s ON s.security_id = k.security_id;

-- Every instrument key and the security that holds it now. A security's key is its least
-- key, so it changes when securities merge (10.2); this view maps any key seen to today's.
CREATE VIEW security_keys AS
SELECT k.key, s.canonical_key AS security_key
FROM instrument_key k JOIN security s ON s.security_id = k.security_id;

CREATE VIEW entries AS
SELECT e.ref, e.account_id, a.masked AS account,
       CASE WHEN e.txn_id IS NOT NULL THEN 'api' ELSE 'file' END AS source,
       e.day, e.posted_date, e.time, e.kind, e.description,
       e.amount_cents, e.fees_cents, e.gross_cents, e.quantity_micros, e.quantity_as_of, e.price,
       e.symbol, (SELECT s.canonical_key FROM security s WHERE s.security_id = e.security_id) AS security_key,
       e.category, e.category_rule, e.pair_kind,
       (SELECT p.ref FROM entry p WHERE p.entry_id = e.pair_entry_id) AS pair_ref,
       (SELECT p.account_id FROM entry p WHERE p.entry_id = e.pair_entry_id) AS pair_account_id,
       e.counted, e.counted_why,
       (SELECT group_concat(code, ',') FROM (SELECT f.code FROM flag f WHERE f.entry_id = e.entry_id ORDER BY f.code)) AS flags,
       (SELECT group_concat(o.ref, ',') FROM (
          SELECT x.ref FROM match_member me JOIN match_member mo ON mo.match_id = me.match_id
                       JOIN entry x ON (x.txn_id = mo.txn_id OR x.csv_txn_id = mo.csv_txn_id)
          WHERE (me.txn_id = e.txn_id OR me.csv_txn_id = e.csv_txn_id) AND x.entry_id <> e.entry_id
          ORDER BY x.ref) o) AS matched_with
FROM entry e JOIN account a ON a.account_id = e.account_id;

CREATE VIEW ledger AS
SELECT ref, account_id, account, source, day, time, kind, description, amount_cents, fees_cents, gross_cents,
       quantity_micros, quantity_as_of, price, symbol, security_key, category, pair_kind, pair_ref,
       pair_account_id, flags
FROM entries WHERE counted = 1;

CREATE VIEW transactions AS
SELECT e.ref, x.account_id, a.masked AS account, x.activity_id, x.time, x.trade_day,
       x.settlement_day, x.type, x.status, x.sub_account, x.description, x.net_cents, x.order_id,
       x.position_id, x.qualified_dividend, e.category, e.category_rule, e.symbol,
       (SELECT s.canonical_key FROM security s WHERE s.security_id = e.security_id) AS security_key,
       e.quantity_micros, e.price, e.gross_cents, e.fees_cents, x.revisions,
       (SELECT r.answered_at FROM api_read r WHERE r.read_id = x.gone_read_id) AS gone_since,
       t.first_seen_at,
       (SELECT CASE r.endpoint WHEN 'file' THEN 'file' ELSE 'api' END FROM api_read r
         WHERE r.read_id = t.first_read_id) AS first_seen_by,   -- 'file': first_seen_at is when you imported it
       t.latest_seen_at, e.counted, e.counted_why,
       (SELECT group_concat(code, ',') FROM (SELECT f.code FROM flag f WHERE f.entry_id = e.entry_id ORDER BY f.code)) AS flags
FROM api_txn t JOIN txn x ON x.txn_id = t.txn_id JOIN account a ON a.account_id = t.account_id
JOIN entry e ON e.txn_id = t.txn_id;

CREATE VIEW revisions AS
SELECT e.ref, t.account_id, v.seq, v.first_seen_at, v.last_seen_at,
       (SELECT CASE r.endpoint WHEN 'file' THEN 'file' ELSE 'api' END FROM api_read r
         WHERE r.read_id = v.first_read_id) AS first_seen_by,
       c.changed
FROM api_revision v JOIN api_txn t ON t.txn_id = v.txn_id JOIN entry e ON e.txn_id = t.txn_id
LEFT JOIN revision_cut c ON c.revision_id = v.revision_id;

-- One row per API leg, and synthesized legs for file transactions (14.8): a file transaction
-- with a quantity has leg 0 its security (cost = gross) and leg 1 its fees when there are any;
-- one without a quantity has one CURRENCY leg holding its Amount, as an API cash row does.
-- quantity_micros is a security leg's shares or contracts; cash_cents a CURRENCY leg's amount
-- (the fee on a trade's fee leg, the cash on a cash row); txn_symbol and txn_security_key are
-- the transaction's (a dividend's ~SYM, a file row's Symbol), which no CURRENCY leg carries.
CREATE VIEW legs AS
SELECT e.ref, e.account_id, a.masked AS account, e.day, e.time, 'api' AS source, l.leg, l.asset_type,
       l.symbol, s.canonical_key AS security_key, l.instrument_id, NULL AS item_issue_id,
       CASE WHEN l.asset_type = 'CURRENCY' THEN NULL ELSE l.amount_micros END AS quantity_micros,
       l.amount_cents AS cash_cents, l.cost_cents, l.price, l.fee_type, l.position_effect,
       s.put_call, s.strike, s.expiration_day, s.multiplier, s.underlying_symbol,
       e.symbol AS txn_symbol,
       (SELECT k.canonical_key FROM security k WHERE k.security_id = e.security_id) AS txn_security_key,
       e.quantity_as_of, e.category, e.counted
FROM api_leg l JOIN entry e ON e.txn_id = l.txn_id JOIN account a ON a.account_id = e.account_id
LEFT JOIN security s ON s.security_id = l.security_id
UNION ALL
SELECT e.ref, e.account_id, a.masked, e.day, e.time, 'file', 0, s.asset_type, NULLIF(e.symbol, ''),
       s.canonical_key, NULL, r.item_issue_id, e.quantity_micros, NULL, e.gross_cents, e.price, NULL, NULL,
       s.put_call, s.strike, s.expiration_day, s.multiplier, s.underlying_symbol,
       NULLIF(e.symbol, ''), s.canonical_key, e.quantity_as_of, e.category, e.counted
FROM entry e JOIN account a ON a.account_id = e.account_id
JOIN csv_txn c ON c.csv_txn_id = e.csv_txn_id JOIN history_row r ON r.row_id = c.shown_row_id
LEFT JOIN security s ON s.security_id = e.security_id
WHERE e.quantity_micros IS NOT NULL
UNION ALL
SELECT e.ref, e.account_id, a.masked, e.day, e.time, 'file', CASE WHEN e.quantity_micros IS NULL THEN 0 ELSE 1 END,
       'CURRENCY', NULL, NULL, NULL, NULL, NULL,
       CASE WHEN e.quantity_micros IS NULL THEN e.amount_cents ELSE e.fees_cents END,
       CASE WHEN e.quantity_micros IS NULL THEN 0 ELSE -e.fees_cents END,
       NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
       NULLIF(e.symbol, ''), (SELECT k.canonical_key FROM security k WHERE k.security_id = e.security_id),
       e.quantity_as_of, e.category, e.counted
FROM entry e JOIN account a ON a.account_id = e.account_id
WHERE e.csv_txn_id IS NOT NULL
  AND (e.quantity_micros IS NULL OR (e.fees_cents IS NOT NULL AND e.fees_cents <> 0));

CREATE VIEW file_transactions AS
SELECT e.ref, c.account_id, a.masked AS account, c.posted_date, c.asof_date, c.event_date,
       r.action, r.symbol, r.description, r.quantity_micros, c.quantity_as_of, r.price, r.fees_cents, r.amount_cents,
       r.item_issue_id, r.acctg_rule_cd, c.files,
       (SELECT s.canonical_key FROM security s WHERE s.security_id = e.security_id) AS security_key,
       e.category, e.category_rule, e.counted, e.counted_why,
       (SELECT group_concat(code, ',') FROM (SELECT f.code FROM flag f WHERE f.entry_id = e.entry_id ORDER BY f.code)) AS flags
FROM csv_txn c JOIN account a ON a.account_id = c.account_id
JOIN history_row r ON r.row_id = c.shown_row_id
JOIN entry e ON e.csv_txn_id = c.csv_txn_id;

CREATE VIEW matches AS
SELECT fe.ref AS file_ref, ae.ref AS api_ref, m.account_id, a.masked AS account, m.method, m.score
FROM match m JOIN account a ON a.account_id = m.account_id
JOIN match_member fm ON fm.match_id = m.match_id AND fm.csv_txn_id IS NOT NULL
JOIN entry fe ON fe.csv_txn_id = fm.csv_txn_id
JOIN match_member am ON am.match_id = m.match_id AND am.txn_id IS NOT NULL
JOIN entry ae ON ae.txn_id = am.txn_id;

CREATE VIEW snapshots AS
SELECT s.snapshot_id, 'S' || s.snapshot_id AS ref, f.account_id, a.masked AS account, s.fetched_at,
       f.market_date, f.in_session_date, s.source, f.account_type,
       f.current_liquidation_cents, f.initial_liquidation_cents, f.aggregated_liquidation_cents, f.cash_cents,
       (SELECT count(*) FROM snapshot_position p WHERE p.snapshot_id = s.snapshot_id) AS positions,
       (SELECT group_concat(code, ',') FROM (SELECT x.code FROM flag x WHERE x.snapshot_id = s.snapshot_id ORDER BY x.code)) AS flags
FROM snapshot s JOIN snapshot_fields f ON f.snapshot_id = s.snapshot_id
JOIN account a ON a.account_id = f.account_id;

CREATE VIEW balances AS
SELECT f.snapshot_id, f.account_id, a.masked AS account, f.fetched_at, f.market_date, f.in_session_date,
       sets.key AS balance_set, fig.key AS figure,
       fig.value ->> 'v' AS value_as_sent, fig.value ->> 'c' AS value_cents
FROM snapshot_fields f JOIN account a ON a.account_id = f.account_id,
     json_each(f.balances) AS sets, json_each(sets.value) AS fig;

CREATE VIEW positions AS
SELECT p.snapshot_id, f.account_id, a.masked AS account, f.fetched_at, f.market_date, f.in_session_date,
       p.position,
       (SELECT k.canonical_key FROM security k WHERE k.security_id = p.security_id) AS security_key,
       p.asset_type, p.symbol, p.cusip, p.description, p.long_micros, p.short_micros, p.settled_long_micros,
       p.settled_short_micros, p.average_price, p.market_value, p.long_open_pl, p.short_open_pl,
       CASE WHEN p.long_micros > 0 THEN p.market_value - p.long_open_pl
            WHEN p.short_micros > 0 THEN p.short_open_pl - p.market_value END AS cost_basis,
       p.maintenance, p.day_pl
FROM snapshot_position p JOIN snapshot_fields f ON f.snapshot_id = p.snapshot_id
JOIN account a ON a.account_id = f.account_id;

CREATE VIEW sessions AS SELECT date, open_at, close_at, early, verified FROM market_session;

-- A gap: a session from an account's first to its last closed-phase snapshot with no
-- closed-phase snapshot of that account standing for it. Sessions after the last one are
-- reported by `schwabtx status`, which knows the time; this view does not depend on it.
CREATE VIEW snapshot_gaps AS
WITH span AS (
  SELECT account_id, min(market_date) AS first_date, max(market_date) AS last_date
  FROM snapshot_fields
  WHERE account_id IS NOT NULL AND market_date IS NOT NULL AND in_session_date IS NULL
  GROUP BY account_id
)
SELECT span.account_id, a.masked AS account, m.date AS market_date
FROM span JOIN account a ON a.account_id = span.account_id
JOIN market_session m ON m.date BETWEEN span.first_date AND span.last_date
WHERE NOT EXISTS (SELECT 1 FROM snapshot_fields x
                   WHERE x.account_id = span.account_id AND x.market_date = m.date AND x.in_session_date IS NULL);

CREATE VIEW coverages AS
SELECT c.account_id, a.masked AS account, c.source, c.from_date, c.to_date
FROM coverage c JOIN account a ON a.account_id = c.account_id;

CREATE VIEW files AS
SELECT d.ref, d.account_id,
       (SELECT a.masked FROM account a WHERE a.account_id = d.account_id) AS account, f.kind,
       d.label, d.layout, f.imported_at, d.exported_at, d.covers_from, d.covers_to, d.coverage_source,
       d.completeness, d.complete_by, d.lacks_rows, d.row_count, d.forgotten,
       (SELECT group_concat(code, ',') FROM (SELECT x.code FROM flag x WHERE x.file_id = f.file_id ORDER BY x.code)) AS warnings
FROM import_file f JOIN file_fields d ON d.file_id = f.file_id;

CREATE VIEW realized_lots AS
SELECT l.ref, l.account_id, a.masked AS account, r.symbol, r.name,
       (SELECT s.canonical_key FROM security s WHERE s.security_id = l.security_id) AS security_key,
       r.closed_date, r.opened_date, r.opened_text, r.quantity_micros, r.proceeds_cents, r.cost_basis_cents,
       r.gain_cents, r.lt_gain_cents, r.st_gain_cents, r.term, r.wash_sale, r.disallowed_loss_cents,
       r.unadjusted_cost_cents, r.txn_cost_basis_cents, r.txn_gain_cents, l.versions, l.files,
       (SELECT group_concat(code, ',') FROM (SELECT f.code FROM flag f WHERE f.lot_id = l.lot_id ORDER BY f.code)) AS flags
FROM realized_lot l JOIN account a ON a.account_id = l.account_id
JOIN realized_row r ON r.row_id = l.shown_row_id;

CREATE VIEW flags AS
SELECT f.code, d.severity, d.meaning, f.detail,
       CASE WHEN f.entry_id IS NOT NULL THEN e.ref
            WHEN f.snapshot_id IS NOT NULL THEN 'S' || f.snapshot_id
            WHEN f.file_id IS NOT NULL THEN i.ref
            ELSE l.ref END AS subject_ref,
       coalesce(e.account_id, s.account_id, i.account_id, l.account_id) AS account_id
FROM flag f JOIN flag_def d ON d.code = f.code
LEFT JOIN entry e ON e.entry_id = f.entry_id
LEFT JOIN snapshot_fields s ON s.snapshot_id = f.snapshot_id
LEFT JOIN file_fields i ON i.file_id = f.file_id
LEFT JOIN realized_lot l ON l.lot_id = f.lot_id;

CREATE VIEW reads AS
SELECT r.read_id, r.endpoint, r.account_id, r.window_start, r.window_end, r.all_types, r.symbol,
       r.sent_at, r.answered_at, r.outcome, r.http_status, r.row_count, r.capped, r.complete
FROM api_read r;
