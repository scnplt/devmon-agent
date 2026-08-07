package state

// currentSchemaVersion is the schema this build understands. A store reporting a
// higher number was written by a newer agent and must not be opened: a downgrade
// that silently ignored unknown columns would corrupt the device registry.
const currentSchemaVersion = 1

// schemaVersionKey is the schema_meta row holding the version number.
const schemaVersionKey = "version"

// schemaV1 is the complete v1 schema.
//
// The devices and audit tables are created now even though nothing writes to
// them until Phases 2 and 5. Creating them here means those phases add rows, not
// migrations, and the whole retention story stays validated in one place.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Populated in Phase 2 (pairing). Created now so Phase 2 needs no migration.
CREATE TABLE IF NOT EXISTS devices (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    cert_serial    TEXT NOT NULL UNIQUE,
    cert_not_after INTEGER NOT NULL,
    paired_at      INTEGER NOT NULL,
    last_seen_at   INTEGER,
    revoked_at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_devices_serial  ON devices(cert_serial);
CREATE INDEX IF NOT EXISTS idx_devices_revoked ON devices(revoked_at);

-- Written in Phase 5. Created and pruned now: the audit trail lives in the
-- database rather than in logs/ so the host-side CLI can read it while the
-- agent writes, and so retention is an indexed DELETE rather than file surgery.
CREATE TABLE IF NOT EXISTS audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at INTEGER NOT NULL,
    device_id   TEXT,
    operation   TEXT NOT NULL,
    target      TEXT,
    outcome     TEXT NOT NULL,
    detail      TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_occurred ON audit(occurred_at);
`
