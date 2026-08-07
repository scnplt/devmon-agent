package state

// currentSchemaVersion is the schema this build understands. A store reporting a
// higher number was written by a newer agent and must not be opened: a downgrade
// that silently ignored unknown columns would corrupt the device registry.
const currentSchemaVersion = 2

// schemaVersionKey is the schema_meta row holding the version number.
const schemaVersionKey = "version"

// schemaMetaTableSQL creates the version-tracking table used by the migration
// ladder. It runs once, ahead of every migration, so the ladder can read a
// version even on a brand-new file where no migration has run yet.
const schemaMetaTableSQL = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// schemaV1 is the complete v1 schema.
//
// The devices and audit tables were created here even though nothing wrote to
// them until Phase 2. That bet only half paid off: v2 (below) has to DROP and
// recreate devices because its cert_serial/cert_not_after columns did not
// survive contact with the real design (certificates now live in their own
// table so a renewal is an INSERT, not a destructive UPDATE — see D6 in the
// Phase 2 plan). audit, which Phase 5 still owns unmodified, needed no
// migration at all — so the bet was half right, not wrong.
const schemaV1 = `
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

// schemaV2 replaces devices with the Phase 2 design and adds the certificate
// and pairing-code registries.
//
// devices is dropped and recreated rather than ALTERed. That is safe, and is
// the only time it will be: v1 never wrote a device row (pairing did not
// exist yet), so every real deployment's table is provably empty at this
// point. Preserving data that cannot exist is complexity for nothing.
const schemaV2 = `
DROP TABLE IF EXISTS devices;

CREATE TABLE devices (
    id           TEXT PRIMARY KEY,   -- 16 lowercase hex chars
    name         TEXT NOT NULL,
    paired_at    INTEGER NOT NULL,
    last_seen_at INTEGER,
    revoked_at   INTEGER             -- NULL = active
);
CREATE INDEX idx_devices_revoked ON devices(revoked_at);

-- device_certs: every certificate ever issued to a device. Auth resolves the
-- peer's serial here, then checks the owning device is not revoked.
CREATE TABLE device_certs (
    serial        TEXT PRIMARY KEY,  -- big.Int.Text(16), lowercase, no 0x
    device_id     TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    not_before    INTEGER NOT NULL,
    not_after     INTEGER NOT NULL,
    issued_at     INTEGER NOT NULL,
    superseded_at INTEGER             -- set on renewal; the cert stays valid (D6)
);
CREATE INDEX idx_device_certs_device ON device_certs(device_id);

-- pairing_codes: pending single-use codes. The code is never stored in
-- plaintext (D4); only its hash is persisted.
CREATE TABLE pairing_codes (
    code_hash   TEXT PRIMARY KEY,    -- hex SHA-256 of the code
    device_name TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,             -- NULL = unused; set atomically on redemption
    used_by     TEXT                 -- device id, once redeemed
);
CREATE INDEX idx_pairing_codes_expires ON pairing_codes(expires_at);
`

// migrationStep is one rung of the migration ladder: the schema statements
// that bring the store from the previous version to version.
type migrationStep struct {
	version int
	sql     string
}

// migrations lists every migration in order. verifyAndMigrate applies every
// step whose version exceeds the store's current version, each in its own
// transaction that also stamps schema_meta.version — so a fresh store walks
// v1 then v2, and a v1 store on disk walks only v2.
var migrations = []migrationStep{
	{version: 1, sql: schemaV1},
	{version: 2, sql: schemaV2},
}
