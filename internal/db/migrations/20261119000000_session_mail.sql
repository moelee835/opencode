-- +goose Up
CREATE TABLE session_mail (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    sender_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    recipient_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    body TEXT NOT NULL CHECK (length(body) > 0),
    request_key TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    read_at INTEGER,
    UNIQUE(sender_id, request_key)
);
CREATE INDEX session_mail_inbox ON session_mail(recipient_id, seq);

-- +goose Down
DROP TABLE session_mail;
