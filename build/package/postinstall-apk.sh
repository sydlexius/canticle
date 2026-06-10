#!/bin/sh
set -e

# Create system group if it does not exist.
if ! getent group mxlrcgo-svc > /dev/null 2>&1; then
    addgroup -S mxlrcgo-svc
fi

# Create system user if it does not exist.
if ! getent passwd mxlrcgo-svc > /dev/null 2>&1; then
    adduser \
        -S \
        -G mxlrcgo-svc \
        -h /var/lib/mxlrcgo-svc \
        -H \
        -s /sbin/nologin \
        mxlrcgo-svc
fi

# Create state, runtime, and config directories.
mkdir -p /var/lib/mxlrcgo-svc /run/mxlrcgo-svc /etc/mxlrcgo-svc
chown mxlrcgo-svc:mxlrcgo-svc /var/lib/mxlrcgo-svc /run/mxlrcgo-svc
chmod 0750 /var/lib/mxlrcgo-svc /run/mxlrcgo-svc

# Do NOT add to default runlevel or start the service.
