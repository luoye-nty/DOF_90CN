package sqlite

const schemaSQL = `
CREATE TABLE IF NOT EXISTS dnf_repository_meta (
    meta_key TEXT PRIMARY KEY,
    meta_value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dnf_repository_records (
    record_kind TEXT NOT NULL,
    record_key TEXT NOT NULL,
    payload BLOB NOT NULL,
    checksum TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (record_kind, record_key)
);
`
