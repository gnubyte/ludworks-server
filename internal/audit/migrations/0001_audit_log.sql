-- Audit log of auth events and admin actions. actor_username is denormalised so entries stay
-- readable after the user is deleted (no foreign key on purpose).
CREATE TABLE audit_log (
    id             TEXT PRIMARY KEY,
    time           TEXT NOT NULL,
    actor_id       TEXT NOT NULL DEFAULT '',
    actor_username TEXT NOT NULL DEFAULT '',
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL DEFAULT '',
    target_id      TEXT NOT NULL DEFAULT '',
    ip             TEXT NOT NULL DEFAULT '',
    user_agent     TEXT NOT NULL DEFAULT '',
    request_id     TEXT NOT NULL DEFAULT '',
    meta           TEXT NOT NULL DEFAULT '{}'
) STRICT;
CREATE INDEX audit_log_time ON audit_log (time DESC, id DESC);
CREATE INDEX audit_log_actor ON audit_log (actor_id, time DESC, id DESC);
CREATE INDEX audit_log_action ON audit_log (action, time DESC, id DESC);
