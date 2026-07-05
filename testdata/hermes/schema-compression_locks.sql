CREATE TABLE compression_locks (
    session_id TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    acquired_at REAL NOT NULL,
    expires_at REAL NOT NULL
);
CREATE INDEX idx_compression_locks_expires ON compression_locks(expires_at);
