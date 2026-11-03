#!/bin/sh
set -eu
INSTALL_ID=$1
WORK_DIR="/var/tmp/speedtest-installer-$INSTALL_ID"
CONFIG_FILE="$WORK_DIR/config.env"
rm -rf "$WORK_DIR" "$CONFIG_FILE"
