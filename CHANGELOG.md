# Changelog

All notable changes to Back! It! Up! are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [0.1.0.0] - 2026-06-22

### Added

- **Credential rotation** — admins can now regenerate an existing client's SSH keypair and bearer token from the client detail page (`POST /clients/{id}/rotate`). The old credentials are invalidated atomically; run history and all other settings are preserved.
- Rotation result page re-uses the one-time secrets display, showing the new private key, token, and cron line — with a visible warning when the SSH `authorized_keys` file could not be updated immediately (credentials remain valid in the database; `authorized_keys` self-heals on next client edit or server restart).
- Schema migration: a `version` column (INTEGER NOT NULL DEFAULT 1) is added to the clients table. Existing databases are upgraded automatically on first startup. This column guards against double-submit races by using compare-and-swap semantics on every rotation — a 409 Conflict is returned when two concurrent rotation requests are detected.

### Changed

- SSH key generation and token hashing extracted into a shared `generateClientCreds` helper used by both client creation and rotation, eliminating duplicate credential-generation code.
- The `RotateClientCreds` store method now accepts the caller's observed `version` and only updates the row when it matches, incrementing the version atomically on success.
