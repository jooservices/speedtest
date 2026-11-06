#!/usr/bin/env bash
# Refresh the offline geolocation databases on the production host from DB-IP "IP to City/ASN Lite" (CC BY 4.0, no key;
# attribution: "IP Geolocation by DB-IP", https://db-ip.com). Same MMDB format as GeoLite2. Run monthly from the repo root:
#   deploy/prod/geoip.sh [YYYY-MM]
set -euo pipefail
HOST="${PROD_HOST:-joos@192.168.10.10}"
MONTH="${1:-$(date -u +%Y-%m)}"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
for kind in city asn; do
	curl -fsS -m 300 -o "$tmp/$kind.mmdb.gz" "https://download.db-ip.com/free/dbip-$kind-lite-$MONTH.mmdb.gz"
	gunzip "$tmp/$kind.mmdb.gz"
done
ssh -o BatchMode=yes "$HOST" "sudo install -d -m 755 /opt/speedtest/geoip"
for kind in city asn; do
	name=GeoLite2-$( [ $kind = city ] && echo City || echo ASN ).mmdb
	scp -q "$tmp/$kind.mmdb" "$HOST:/tmp/$name" && ssh -o BatchMode=yes "$HOST" "sudo install -m 644 /tmp/$name /opt/speedtest/geoip/$name && rm -f /tmp/$name"
done
echo "installed; the center reopens the files when they change (or restart: deploy/prod/prod.sh deploy)"
