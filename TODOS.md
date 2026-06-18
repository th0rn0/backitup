# backitup — TODOS

## Client credential lifecycle (rotation / bootstrap)
- **What:** A `backitup rotate-client <id>` flow that reissues a client's SSH key +
  bearer token and surfaces the new secret + cron line to redeploy on the host.
- **Why:** Client creds (SSH key, bearer token, pinned CA) live on each host. v1 ships
  manual issue (the webgui shows the secret once at creation). There's no rotation story:
  rotating today means delete + re-add the client and re-paste on the host. For a real
  fleet, key/token rotation without churning every host by hand is the actual ops pain.
- **Pros:** Clean fleet operations; supports compromise response (rotate a leaked token)
  without losing the client's backup history.
- **Cons:** Extra surface; needs a way to push/deploy the new secret to the host (or at
  least a clear copy-paste flow). At homelab scale, manual re-add is tolerable for a while.
- **Context:** v1 decision (D3/D4) is built-in admin login issuing per-client SSH keypair
  + bearer token, written into sshd authorized_keys via shared volume. Rotation reuses
  that issuance path; the missing piece is reissue + redeploy UX. See design doc Eng
  Review Decisions D4, D10.
- **Depends on:** v1 client issuance + key-sync seam (D4) shipped first.
