# backitup — TODOS

## ~~Client credential lifecycle (rotation / bootstrap)~~ ✅ DONE — v0.1.0.0
Shipped in `feat/client-credential-rotation`. `POST /clients/{id}/rotate` reissues SSH keypair + bearer token atomically. Old credentials invalidated immediately; new secrets surfaced once in the UI. Compare-and-swap version column guards against concurrent rotation races.

## User system and user management
Simple multi-user access to the web UI. No roles or permissions — every user has full access. Needs:
- User table in DB (username, password hash)
- Create/delete users in the UI
- Login tied to a user record rather than the single `BACKITUP_ADMIN_USER`/`BACKITUP_ADMIN_PASSWORD` env vars
- Sessions/tokens invalidated on password change or user deletion

## WebUI redesign — live backup progress
The dashboard and client detail pages should auto-update without a full page reload. Key requirements:
- Show when a backup is actively running (in-progress status)
- Show real-time progress during a backup (files transferred, bytes, percentage if possible)
- Dashboard auto-refreshes to reflect latest run status across all clients
- No manual page reload needed to see a backup complete

Implementation ideas: SSE or polling endpoint that streams current run state; client posts incremental progress updates during the backup; server holds in-memory run state that the UI polls every few seconds.
