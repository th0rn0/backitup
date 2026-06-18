#!/bin/sh
set -eu

# sshd needs its privilege-separation dir.
mkdir -p /run/sshd /srv/hostkeys

# Generate the host key once, on a persisted volume, so clients don't see
# host-key-changed warnings after a restart.
if [ ! -f /srv/hostkeys/ssh_host_ed25519_key ]; then
  ssh-keygen -t ed25519 -f /srv/hostkeys/ssh_host_ed25519_key -N '' >/dev/null
fi
chmod 600 /srv/hostkeys/ssh_host_ed25519_key

# Ensure the shared dirs exist with sane ownership/permissions. The app (same
# uid) writes /srv/authkeys/authorized_keys atomically; sshd StrictModes
# requires it to be non-group/world-writable.
mkdir -p /srv/backups /srv/authkeys
touch /srv/authkeys/authorized_keys
chmod 700 /srv/authkeys
chmod 600 /srv/authkeys/authorized_keys 2>/dev/null || true
chown -R backitup /srv/backups 2>/dev/null || true

# Validate config, then run in the foreground.
/usr/sbin/sshd -t -f /etc/ssh/sshd_config
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
