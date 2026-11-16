# Changelog

All notable changes to Back! It! Up! are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Security

- **Client directories can no longer be shared.** A client's storage directory was derived on the fly from its name, so names differing only in punctuation, spacing or case (`dup a`, `dup-a`, `Dup_A`, `dup/a`) produced one directory and one identical SSH forced command — two clients in one read-write jail, each able to download the other's data. A name with no alphanumeric characters (`..`, `.`, `/`) slugged to `""`, which jailed the client at the backup base directory itself. The slug is now a persisted, UNIQUE column (`clients.slug`), computed once on create and used for every directory path (`authorized_keys`, snapshot listing/download/delete, offsite object prefix, lifecycle pruning). `POST /clients` rejects an empty slug, a slug already in use, and a name longer than 63 characters (the name becomes the directory component and part of the sshd forced command) with a form error naming the conflict; `authkeys.Render` emits no line at all for a client whose slug is unusable, and the slug is validated against an allowlist (`^[a-z0-9][a-z0-9-]{0,62}$`) because it is embedded in `authorized_keys`. Existing databases are upgraded automatically: each row is backfilled from its name, the oldest client keeps the plain slug, later collisions get `-2`, `-3`, … and a slugless name falls back to `client-<id>`. **This separates future writes only — see the upgrade note below for clients that already shared a directory.**
- **Creating a client can no longer break SSH for the fleet, and deleting one can no longer leave its key live.** A backup directory that cannot be created (an over-long name made every `MkdirAll` fail) used to abort the whole `authorized_keys` rewrite, so after that point every create/rotate/delete only logged an error while the file stayed stale — a deleted client kept SSH write access. The name is now capped at 63 characters when the client is created, a directory failure is reported per client without blocking the rewrite, and a failed rewrite after a delete is shown to the operator as an error instead of a success redirect.
- **Deleting a user now ends their sessions.** Admin sessions were keyed only by the username string and never re-checked, so a deleted account kept full admin access until the 12-hour session TTL expired — enough to mint new client keys. Sessions are now bound to the user id, `postDeleteUser` revokes every session of the deleted account, and `requireAdmin` treats a session whose user no longer exists as invalid.

### Upgrade notes

- **The client-directory split is forward-only; pre-existing colliding clients keep sharing one directory.** Before this release a client's directory came from its name, so two clients whose names slugged alike (`dup a` and `dup-a`) wrote into *one* directory. The migration gives the later client a new slug, which changes where it writes *from now on*, but it cannot move its history: snapshots in that directory were written by one ssh user with timestamp-only ids, so they are not attributable to either client, and an automatic move would guess and could delete data. Left as-is, the client that kept the plain slug still reads and writes the shared directory and prunes it under its own retention policy, and the other client's existing snapshots there are stranded (never listed, pruned or uploaded again); its offsite default prefix also changes (`dup-a` → `dup-a-2`), so its existing cold objects are reported `remote_missing`. **On upgrade, check the server log for `WARNING: legacy client slug collision`** — one line per affected pair, naming both clients, the retained slug, the new slug and the shared directory. To quarantine it: stop the server, rename that directory aside (e.g. `mv /srv/backups/dup-a /srv/backups/dup-a.quarantine`), start the server so the remaining client begins with an empty directory, then inspect the renamed copy by hand and re-upload whichever client's data you keep.

## [0.1.0.0] - 2026-06-22

### Added

- **Credential rotation** — admins can now regenerate an existing client's SSH keypair and bearer token from the client detail page (`POST /clients/{id}/rotate`). The old credentials are invalidated atomically; run history and all other settings are preserved.
- Rotation result page re-uses the one-time secrets display, showing the new private key, token, and cron line — with a visible warning when the SSH `authorized_keys` file could not be updated immediately (credentials remain valid in the database; `authorized_keys` self-heals on next client edit or server restart).
- Schema migration: a `version` column (INTEGER NOT NULL DEFAULT 1) is added to the clients table. Existing databases are upgraded automatically on first startup. This column guards against double-submit races by using compare-and-swap semantics on every rotation — a 409 Conflict is returned when two concurrent rotation requests are detected.

### Changed

- SSH key generation and token hashing extracted into a shared `generateClientCreds` helper used by both client creation and rotation, eliminating duplicate credential-generation code.
- The `RotateClientCreds` store method now accepts the caller's observed `version` and only updates the row when it matches, incrementing the version atomically on success.
