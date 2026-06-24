#!/bin/sh
# Entrypoint for the backitup client container.
#
# OpenSSH calls getpwuid() unconditionally at startup. When the container is
# launched with --user <uid>:<gid> and that uid has no /etc/passwd entry,
# SSH exits immediately with "No user exists for uid N". This script adds a
# minimal passwd entry for the current uid before handing off to the client
# binary, making the image safe with arbitrary --user values.
set -e
if ! whoami >/dev/null 2>&1; then
    echo "backitup:x:$(id -u):$(id -g)::/tmp:/sbin/nologin" >> /etc/passwd
fi
exec "$@"
