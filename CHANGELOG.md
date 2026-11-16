# Changelog

All notable changes to Back! It! Up! are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Security

- **Client directories can no longer be shared.** A client's storage directory was derived on the fly from its name, so names differing only in punctuation, spacing or case (`dup a`, `dup-a`, `Dup_A`, `dup/a`) produced one directory and one identical SSH forced command — two clients in one read-write jail, each able to download the other's data. A name with no alphanumeric characters (`..`, `.`, `/`) slugged to `""`, which jailed the client at the backup base directory itself. The slug is now a persisted, UNIQUE column (`clients.slug`), computed once on create and used for every directory path (`authorized_keys`, snapshot listing/download/delete, offsite object prefix, lifecycle pruning). `POST /clients` rejects an empty slug and a slug already in use with a form error naming the conflict; `authkeys.Render` emits no line at all for a client whose slug is unusable. Existing databases are upgraded automatically: each row is backfilled from its name, the oldest client keeps the plain slug, later collisions get `-2`, `-3`, … and a slugless name falls back to `client-<id>`.
- **Deleting a user now ends their sessions.** Admin sessions were keyed only by the username string and never re-checked, so a deleted account kept full admin access until the 12-hour session TTL expired — enough to mint new client keys. Sessions are now bound to the user id, `postDeleteUser` revokes every session of the deleted account, and `requireAdmin` treats a session whose user no longer exists as invalid.

## [0.1.0.0] - 2026-06-22

### Added

- **Credential rotation** — admins can now regenerate an existing client's SSH keypair and bearer token from the client detail page (`POST /clients/{id}/rotate`). The old credentials are invalidated atomically; run history and all other settings are preserved.
- Rotation result page re-uses the one-time secrets display, showing the new private key, token, and cron line — with a visible warning when the SSH `authorized_keys` file could not be updated immediately (credentials remain valid in the database; `authorized_keys` self-heals on next client edit or server restart).
- Schema migration: a `version` column (INTEGER NOT NULL DEFAULT 1) is added to the clients table. Existing databases are upgraded automatically on first startup. This column guards against double-submit races by using compare-and-swap semantics on every rotation — a 409 Conflict is returned when two concurrent rotation requests are detected.

### Changed

- SSH key generation and token hashing extracted into a shared `generateClientCreds` helper used by both client creation and rotation, eliminating duplicate credential-generation code.
- The `RotateClientCreds` store method now accepts the caller's observed `version` and only updates the row when it matches, incrementing the version atomically on success.
