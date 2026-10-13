package store

// migrations are applied in order; PRAGMA user_version records how many ran.
// Never edit a released migration, append a new one.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE accounts (
  id                 INTEGER PRIMARY KEY,
  user               TEXT NOT NULL UNIQUE,          -- lower-case login name
  folder_sync_key    TEXT NOT NULL DEFAULT '0',     -- FolderSync key (committed)
  policy_key         TEXT NOT NULL DEFAULT '',      -- Provision policy key (an EAS device token)
  uid_validity_floor INTEGER NOT NULL DEFAULT 0,    -- last UIDVALIDITY handed out
  created_at         INTEGER NOT NULL
);

CREATE TABLE folders (
  id               INTEGER PRIMARY KEY,
  account_id       INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  server_id        TEXT NOT NULL,                   -- EAS folder ServerId
  parent_server_id TEXT NOT NULL DEFAULT '0',
  display_name     TEXT NOT NULL,
  eas_type         INTEGER NOT NULL,
  path             TEXT NOT NULL,                   -- IMAP mailbox name
  special_use      TEXT NOT NULL DEFAULT '',        -- \Sent, \Drafts, \Trash, \Junk, \Archive or ''
  uid_validity     INTEGER NOT NULL,
  uid_next         INTEGER NOT NULL DEFAULT 1,
  sync_key         TEXT NOT NULL DEFAULT '0',       -- committed Sync key
  recon_active     INTEGER NOT NULL DEFAULT 0,      -- 1 while a full resync is being collected
  recon_epoch      INTEGER NOT NULL DEFAULT 0,
  recon_verify_at  INTEGER NOT NULL DEFAULT 0,      -- unix time a verification resync is due, 0 = none
  UNIQUE (account_id, server_id)
);
CREATE INDEX folders_path ON folders(account_id, path);

CREATE TABLE messages (
  id                  INTEGER PRIMARY KEY,
  folder_id           INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  uid                 INTEGER NOT NULL,
  server_id           TEXT,                          -- NULL for bridge-only messages
  kind                INTEGER NOT NULL DEFAULT 0,    -- 0 server, 1 local draft, 2 sent placeholder, 3 local only
  flags               INTEGER NOT NULL DEFAULT 0,    -- bit mask, see Flags
  keywords            TEXT NOT NULL DEFAULT '',      -- space separated, local only
  internal_date       INTEGER NOT NULL,
  size                INTEGER NOT NULL,
  message_id          TEXT NOT NULL DEFAULT '',      -- without angle brackets
  in_reply_to         TEXT NOT NULL DEFAULT '',
  refs                TEXT NOT NULL DEFAULT '',
  subject             TEXT NOT NULL DEFAULT '',
  date_hdr            INTEGER NOT NULL DEFAULT 0,
  to_hdr              TEXT NOT NULL DEFAULT '',
  placeholder_expires INTEGER NOT NULL DEFAULT 0,
  recon_mark          INTEGER NOT NULL DEFAULT 0,
  recon_strikes       INTEGER NOT NULL DEFAULT 0,
  header              BLOB NOT NULL,
  UNIQUE (folder_id, uid)
);
CREATE UNIQUE INDEX messages_server_id ON messages(folder_id, server_id) WHERE server_id IS NOT NULL;
CREATE INDEX messages_message_id ON messages(folder_id, message_id) WHERE message_id != '';
CREATE INDEX messages_kind ON messages(folder_id, kind) WHERE kind != 0;

CREATE TABLE mime (
  message_pk INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  data       BLOB NOT NULL
);

-- In-Reply-To (kind 1) and References (kind 2) tokens, for indexed thread lookups.
CREATE TABLE message_refs (
  message_pk INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  kind       INTEGER NOT NULL,
  ref        TEXT NOT NULL
);
CREATE INDEX message_refs_ref ON message_refs(ref);
CREATE INDEX message_refs_msg ON message_refs(message_pk);

-- Items announced by Sync whose MIME was truncated. They get a UID only once
-- the full MIME has been fetched.
CREATE TABLE pending_fetch (
  id            INTEGER PRIMARY KEY,
  folder_id     INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  server_id     TEXT NOT NULL,
  flags         INTEGER NOT NULL DEFAULT 0,
  internal_date INTEGER NOT NULL,
  est_size      INTEGER NOT NULL DEFAULT 0,
  attempts      INTEGER NOT NULL DEFAULT 0,
  next_attempt  INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  UNIQUE (folder_id, server_id)
);

-- Local changes not yet applied upstream. One row per (item, op); a newer
-- STORE overwrites the value and bumps version.
CREATE TABLE outbox (
  id           INTEGER PRIMARY KEY,
  folder_id    INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  server_id    TEXT NOT NULL,
  op           INTEGER NOT NULL,                     -- 1 read, 2 flagged, 3 delete
  value        INTEGER NOT NULL DEFAULT 0,
  orig         INTEGER NOT NULL DEFAULT -1,          -- value before the first queued change (-1 unknown), for rollback
  hard         INTEGER NOT NULL DEFAULT 0,           -- delete: 1 = DeletesAsMoves=0
  version      INTEGER NOT NULL DEFAULT 1,
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL DEFAULT 0,           -- unix milliseconds
  created_at   INTEGER NOT NULL,
  UNIQUE (folder_id, server_id, op)
);
`,
	// 2: all-or-nothing first sync per folder (v0.1.1).
	//
	// A folder exposes none of its server messages until its first sync has
	// caught up; until then they sit in "messages" as staged rows with a
	// provisional NEGATIVE uid taken from folders.stage_next (so the unique
	// (folder_id, uid) index and every other constraint keep working). Opening
	// the folder renumbers them to real UIDs in date order in one transaction.
	// Every read that feeds IMAP filters on uid > 0.
	`
ALTER TABLE folders ADD COLUMN initial_sync_complete INTEGER NOT NULL DEFAULT 0;
ALTER TABLE folders ADD COLUMN initial_chain_done    INTEGER NOT NULL DEFAULT 0;  -- last data Sync reported no MoreAvailable
ALTER TABLE folders ADD COLUMN stage_next            INTEGER NOT NULL DEFAULT -1; -- next provisional uid for a staged row
ALTER TABLE pending_fetch ADD COLUMN item_failures   INTEGER NOT NULL DEFAULT 0;  -- failures that are about this item, not about Exchange

-- A folder that already handed out messages (or has a sync chain under way)
-- keeps behaving as before: it is never hidden retroactively.
UPDATE folders SET initial_sync_complete = 1, initial_chain_done = 1
 WHERE sync_key != '0' OR uid_next > 1
    OR EXISTS (SELECT 1 FROM messages m WHERE m.folder_id = folders.id)
    OR EXISTS (SELECT 1 FROM pending_fetch p WHERE p.folder_id = folders.id);

CREATE INDEX messages_staged ON messages(folder_id) WHERE uid < 0;
`,
	// 3: the MIME structure and ENVELOPE of every message, computed once when the
	// message is stored (v0.1.2). A bounded, zero-copy index (see
	// internal/mimestruct): FETCH BODYSTRUCTURE and ENVELOPE read this column
	// instead of parsing the message again, and a hostile message cannot make
	// each FETCH expensive. NULL for rows stored before v0.1.2: the index is then
	// built on demand.
	`
ALTER TABLE messages ADD COLUMN structure BLOB;
`,
	// 4: data integrity (v0.1.2).
	//
	// A message the user deletes disappears locally at once and the delete is
	// queued for Exchange. If Exchange refuses it, the message has to come back:
	// the queued delete keeps what is needed to bring it back (its flags and
	// received date; the body is downloaded again). restore_date is 0 for a
	// delete queued before this version, which has nothing to restore from.
	//
	// A message Exchange keeps failing to render as MIME is finally shown as a
	// small placeholder built from the metadata of the Sync response, so the
	// pending row keeps subject, sender and recipients.
	`
ALTER TABLE outbox ADD COLUMN restore_flags INTEGER NOT NULL DEFAULT 0;
ALTER TABLE outbox ADD COLUMN restore_date  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pending_fetch ADD COLUMN subject   TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_fetch ADD COLUMN from_addr TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_fetch ADD COLUMN to_addr   TEXT NOT NULL DEFAULT '';
`,
	// 5: the message as the bridge submitted it, and the Bcc property of a sent
	// message (v0.1.8).
	//
	// SMTP submission hands Exchange the message with its Bcc recipients written
	// into a Bcc header, because EAS reads the recipients from the headers. Exchange
	// delivers it without that header and saves a Sent Items copy without it too, so
	// the Sent folder would show the message without its Bcc recipients. sent_copy
	// keeps the submitted bytes, keyed by Message-ID, until Exchange's own copy has
	// appeared in Sent Items and has been given them (or until it is too old, or too
	// many are kept: see Tx.PruneSentCopies). The messages table then holds the
	// bytes, and the row goes.
	//
	// pending_fetch.bcc is the email2:Bcc property Exchange lists with an item, for
	// a Sent Items message that has to be downloaded before it can be stored.
	`
CREATE TABLE sent_copy (
  id         INTEGER PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  message_id TEXT NOT NULL,                          -- without angle brackets, never empty
  created_at INTEGER NOT NULL,
  data       BLOB NOT NULL,
  UNIQUE (account_id, message_id)
);
CREATE INDEX sent_copy_age ON sent_copy(account_id, created_at);

ALTER TABLE pending_fetch ADD COLUMN bcc TEXT NOT NULL DEFAULT '';
`,
	// 6: contacts and the directory (v0.2.0, docs/DESIGN-contacts.md).
	//
	// contact_folders are the Contacts folders (EAS types 9 and 14) with their own
	// sync key; their cards (contact_cards) are rendered vCards. A download from key 0
	// writes its cards with staged = 1, invisible to CardDAV, and one transaction swaps
	// them in when the download is complete: a book lists nothing until its first full
	// download has finished, and a restarted download replaces the old cards at once.
	// ctag_seq counts the transactions that changed a book's cards (the CardDAV ctag).
	//
	// directory_cards are the Global Address List entries kept for an exact address
	// match, directory_candidates the addresses in the directory domains that appear in
	// stored mail (with how often and when, and where the lookup stands), directory_state
	// the per account bookkeeping: the ctag counter, the seeding progress, the pause after
	// an access denied answer and the search count of the day.
	//
	// Nothing here is filled unless the contacts feature is on (EASBRIDGE_CONTACTS).
	`
CREATE TABLE contact_folders (
  id           INTEGER PRIMARY KEY,
  account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  server_id    TEXT NOT NULL,                      -- EAS folder ServerId
  display_name TEXT NOT NULL,
  eas_type     INTEGER NOT NULL,                   -- 9 default Contacts, 14 user-created
  book_id      TEXT NOT NULL,                      -- the path segment of the CardDAV book
  sync_key     TEXT NOT NULL DEFAULT '0',          -- committed Sync key
  complete     INTEGER NOT NULL DEFAULT 0,         -- the first full download has finished
  downloading  INTEGER NOT NULL DEFAULT 0,         -- a download from key 0 is under way (cards go in staged)
  ctag_seq     INTEGER NOT NULL DEFAULT 0,
  created_ns   INTEGER NOT NULL,
  last_sync_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE (account_id, server_id),
  UNIQUE (account_id, book_id)
);

CREATE TABLE contact_cards (
  id        INTEGER PRIMARY KEY,
  folder_id INTEGER NOT NULL REFERENCES contact_folders(id) ON DELETE CASCADE,
  server_id TEXT NOT NULL,
  uid       TEXT NOT NULL,
  etag      TEXT NOT NULL,
  vcard     BLOB NOT NULL,
  staged    INTEGER NOT NULL DEFAULT 0,
  UNIQUE (folder_id, server_id, staged)
);
CREATE INDEX contact_cards_uid ON contact_cards(folder_id, uid) WHERE staged = 0;

CREATE TABLE directory_cards (
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  email      TEXT NOT NULL,                         -- lower case
  uid        TEXT NOT NULL,
  etag       TEXT NOT NULL,
  vcard      BLOB NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, email)
) WITHOUT ROWID;
CREATE INDEX directory_cards_uid ON directory_cards(account_id, uid);

CREATE TABLE directory_candidates (
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  email      TEXT NOT NULL,                         -- lower case
  name       TEXT NOT NULL DEFAULT '',              -- the display name last seen with it, for the third search
  seen_count INTEGER NOT NULL DEFAULT 1,
  first_seen INTEGER NOT NULL,                      -- unix time of the message
  last_seen  INTEGER NOT NULL,
  state      INTEGER NOT NULL DEFAULT 0,            -- 0 not looked up yet, 1 found, 2 not found
  checked_at INTEGER NOT NULL DEFAULT 0,
  next_check INTEGER NOT NULL DEFAULT 0,            -- due when next_check <= now
  miss_first INTEGER NOT NULL DEFAULT 0,            -- when a found card was first not found again, 0 = no miss
  PRIMARY KEY (account_id, email)
) WITHOUT ROWID;
CREATE INDEX directory_candidates_due ON directory_candidates(account_id, next_check, last_seen);

CREATE TABLE directory_state (
  account_id     INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  created_ns     INTEGER NOT NULL,
  ctag_seq       INTEGER NOT NULL DEFAULT 0,
  seeded_for     TEXT NOT NULL DEFAULT '',          -- the domain list the seeding ran for
  seed_after     INTEGER NOT NULL DEFAULT 0,        -- seeding has read the messages up to this primary key
  seed_done      INTEGER NOT NULL DEFAULT 0,
  paused_until   INTEGER NOT NULL DEFAULT 0,        -- unix time, after an access denied answer
  lookups_day    TEXT NOT NULL DEFAULT '',          -- UTC date of lookups_today
  lookups_today  INTEGER NOT NULL DEFAULT 0,
  last_search_ms INTEGER NOT NULL DEFAULT 0
);
`,
	// 7: the opt-in full sweep of the Global Address List (v0.2.1, docs/DESIGN-contacts.md
	// section 9).
	//
	// directory_sweep is the state of the sweep of one account: the phase (idle,
	// calibrating the shortest query Exchange answers, sweeping), the calibrated length, the
	// counters of the sweep under way and the times that decide when the next one starts.
	// directory_sweep_queue holds the prefixes still to be searched, in order; gen_pos and
	// gen_total walk through the strings of the calibrated length without writing all of
	// them at once.
	//
	// A directory card remembers how it was found (a lookup of an address from mail, the
	// sweep, or both), the last sweep that saw it, and when an exact lookup last confirmed it.
	// Cards that exist when this migration runs were all found by lookups.
	//
	// 0.2.0 was never published, so an upgrade goes from schema 5 to 7 directly; a
	// database of schema 6 gets this migration only.
	`
CREATE TABLE directory_sweep (
  account_id           INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  sweep_id             INTEGER NOT NULL DEFAULT 0,   -- the sweep under way, or the last one
  phase                INTEGER NOT NULL DEFAULT 0,   -- 0 idle, 1 calibrating, 2 sweeping
  calib_next           INTEGER NOT NULL DEFAULT 1,   -- the next query length to try while calibrating
  calib_len            INTEGER NOT NULL DEFAULT 0,   -- the calibrated shortest query length, 0 = none
  calib_failed         INTEGER NOT NULL DEFAULT 0,   -- no length up to 4 was answered
  started_at           INTEGER NOT NULL DEFAULT 0,
  next_start_at        INTEGER NOT NULL DEFAULT 0,   -- the next sweep starts at this time
  searches             INTEGER NOT NULL DEFAULT 0,   -- this sweep
  found                INTEGER NOT NULL DEFAULT 0,   -- distinct entries kept this sweep
  prefixes_done        INTEGER NOT NULL DEFAULT 0,
  capped               INTEGER NOT NULL DEFAULT 0,   -- this sweep hit the depth or the search cap
  gen_pos              INTEGER NOT NULL DEFAULT 0,
  gen_total            INTEGER NOT NULL DEFAULT 0,
  finished_at          INTEGER NOT NULL DEFAULT 0,   -- end of the last finished sweep
  last_capped          INTEGER NOT NULL DEFAULT 0,   -- the last finished sweep was capped
  completed_id         INTEGER NOT NULL DEFAULT 0,   -- the last sweep that finished without a cap
  completed_started_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE directory_sweep_queue (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  prefix     TEXT NOT NULL
);
CREATE INDEX directory_sweep_queue_account ON directory_sweep_queue(account_id, id);

ALTER TABLE directory_cards ADD COLUMN by_lookup    INTEGER NOT NULL DEFAULT 1;
ALTER TABLE directory_cards ADD COLUMN by_sweep     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE directory_cards ADD COLUMN sweep_seen   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE directory_cards ADD COLUMN confirmed_at INTEGER NOT NULL DEFAULT 0;
`,
	// 8: whether the stored hierarchy was taken with the contact folders kept (v0.2.2).
	//
	// A FolderSync key committed while the contacts feature was off (every key of 0.1.8)
	// stands for a hierarchy that dropped the Contacts folders, so an incremental FolderSync
	// never mentions them. contact_folders_kept says the hierarchy behind folder_sync_key
	// was applied with the contact folders kept; the engine reads the hierarchy again from
	// key 0, once, when contacts are on and it is 0. It is 0 for every existing account, so
	// the first start of 0.2.2 with contacts on does that once.
	`
ALTER TABLE accounts ADD COLUMN contact_folders_kept INTEGER NOT NULL DEFAULT 0;
`,
}
