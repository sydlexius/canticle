#!/bin/sh
set -e

# Stop the service if it is running. Failure is non-fatal.
systemctl stop mxlrcgo-svc || true

# State directory (/var/lib/mxlrcgo-svc) and system user are intentionally
# preserved so the SQLite database survives an upgrade or reinstall.
