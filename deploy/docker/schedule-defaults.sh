#!/usr/bin/env bash
# Shared schedule defaults for the staging center and extra-node containers.
staging_interval() { printf '%s' "${STAGING_INTERVAL_MINUTES:-60}"; }
node_interval() { printf '%s' "${NODE_INTERVAL:-60}"; }
