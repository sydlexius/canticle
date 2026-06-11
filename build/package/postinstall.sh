#!/bin/sh
set -e

# Create system group if it does not exist.
if ! getent group mxlrcgo-svc > /dev/null 2>&1; then
    groupadd --system mxlrcgo-svc
fi

# Create system user if it does not exist.
if ! getent passwd mxlrcgo-svc > /dev/null 2>&1; then
    useradd \
        --system \
        --gid mxlrcgo-svc \
        --home-dir /var/lib/mxlrcgo-svc \
        --no-create-home \
        --shell /usr/sbin/nologin \
        mxlrcgo-svc
fi

# Create state directory.
mkdir -p /var/lib/mxlrcgo-svc
chown mxlrcgo-svc:mxlrcgo-svc /var/lib/mxlrcgo-svc
chmod 0750 /var/lib/mxlrcgo-svc

# Create config directory.
mkdir -p /etc/mxlrcgo-svc

# Reload unit files so the new unit is available.
# Do NOT enable or start the service.
systemctl daemon-reload || true
