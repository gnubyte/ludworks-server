-- Mail-in bookkeeping: which messages of the polled mailbox were already handled, so a
-- message is never imported twice (also when marking it \Seen fails or mark_seen is off).
-- account_key is a hash of host, port, username and mailbox (no credentials are stored);
-- rows belong to one UIDVALIDITY and are pruned when it changes or the message is no
-- longer unseen.
--   status 'done'    imported as a note
--           'skipped' not imported for good (unknown sender, DMARC failure, too large, unparseable)
--           'failed'  import failed permanently, or after the retry budget
--           'retry'   a transient failure; retried on the next polls (attempts counts them)
CREATE TABLE mail_in_processed (
    account_key  TEXT    NOT NULL,
    uid_validity INTEGER NOT NULL,
    uid          INTEGER NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('done', 'skipped', 'failed', 'retry')),
    attempts     INTEGER NOT NULL DEFAULT 1,
    note_id      TEXT    NULL,
    processed_at TEXT    NOT NULL,
    PRIMARY KEY (account_key, uid_validity, uid)
) STRICT, WITHOUT ROWID;
