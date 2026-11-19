#!/bin/sh
# Writes the BACKITUP_RECV_* ingest limits into the file backitup-recv reads.
#
# Why a file: sshd starts every forced command with a fresh environment
# (PermitUserEnvironment is off, and the container's own variables never reach a
# session), so exporting these in docker-compose.yml would have no effect at
# all. entrypoint.sh calls this on every start with /etc/backitup/recv-limits.
#
# Usage: recv-limits.sh <output-file>
set -eu

out="${1:?usage: recv-limits.sh <output-file>}"

# Written aside and renamed into place: an upload that starts while the
# container is restarting must never source a half-written file, and the
# previous file is mode 0444 so it cannot be truncated in place.
tmp="$out.tmp"
trap 'rm -f "$tmp"' EXIT
: >"$tmp"

# The file is sourced by a shell, so only whole byte counts may be written: a
# value nobody can parse must not sit there waiting to be interpreted. A bad
# value is dropped with a warning rather than written through, and backitup-recv
# then applies its built-in default.
write_limit() {
  var="$1"
  eval "val=\${$var:-}"
  [ -n "$val" ] || return 0
  case "$val" in
  *[!0-9]*)
    echo "backitup: ignoring $var='$val' (must be a whole number of bytes)" >&2
    return 0
    ;;
  esac
  printf '%s=%s\n' "$var" "$val" >>"$tmp"
}

write_limit BACKITUP_RECV_MAX_BYTES
write_limit BACKITUP_RECV_MIN_FREE_BYTES

# Readable by the unprivileged 'backitup' user, writable by nobody: the limits
# are the container's configuration, not something a client can edit.
chmod 0444 "$tmp"
mv "$tmp" "$out"
