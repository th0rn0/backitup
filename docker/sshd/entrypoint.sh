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

# The app owns /srv/authkeys (mounted read-only here) and writes authorized_keys
# atomically — sshd only reads it, and tolerates it not existing yet (no clients).
# Do NOT write into /srv/authkeys from here; it would fail on the read-only mount.

# Validate config, then run in the foreground.
/usr/sbin/sshd -t -f /etc/ssh/sshd_config
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
