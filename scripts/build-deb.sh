#!/usr/bin/env bash
# Build a hysteria .deb package for linux/amd64.
#
# Prerequisite:
#   python hyperbole.py build -r      # produces build/hysteria-linux-amd64
#
# Usage:
#   scripts/build-deb.sh <version>
#   e.g. scripts/build-deb.sh 2.6.2-kotodian.1
#
# Output:
#   build/hysteria_<version>_amd64.deb

set -euo pipefail

VERSION="${1:?usage: $0 <version> (e.g. 2.6.2-kotodian.1)}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO_ROOT/build/hysteria-linux-amd64"
TEMPLATE="$REPO_ROOT/packaging/deb"

if [ ! -x "$BIN" ]; then
    echo "error: $BIN not found. Run: python hyperbole.py build -r" >&2
    exit 1
fi
if [ ! -d "$TEMPLATE" ]; then
    echo "error: $TEMPLATE not found" >&2
    exit 1
fi

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

install -d -m 0755 "$STAGE/DEBIAN"
install -d -m 0755 "$STAGE/usr/bin"
install -d -m 0755 "$STAGE/lib/systemd/system"
install -d -m 0755 "$STAGE/etc/hysteria"
install -d -m 0755 "$STAGE/etc/sysctl.d"

install -m 0755 "$BIN" "$STAGE/usr/bin/hysteria"

sed "s/__VERSION__/$VERSION/g" "$TEMPLATE/DEBIAN/control" > "$STAGE/DEBIAN/control"
install -m 0755 "$TEMPLATE/DEBIAN/postinst" "$STAGE/DEBIAN/postinst"
install -m 0755 "$TEMPLATE/DEBIAN/prerm"    "$STAGE/DEBIAN/prerm"
install -m 0755 "$TEMPLATE/DEBIAN/postrm"   "$STAGE/DEBIAN/postrm"

install -m 0644 "$TEMPLATE/lib/systemd/system/hysteria-server.service"  "$STAGE/lib/systemd/system/"
install -m 0644 "$TEMPLATE/lib/systemd/system/hysteria-server@.service" "$STAGE/lib/systemd/system/"
install -m 0644 "$TEMPLATE/etc/hysteria/config.yaml.example"            "$STAGE/etc/hysteria/"
install -m 0644 "$TEMPLATE/etc/sysctl.d/99-hysteria.conf"               "$STAGE/etc/sysctl.d/"

OUT="$REPO_ROOT/build/hysteria_${VERSION}_amd64.deb"
dpkg-deb --root-owner-group --build "$STAGE" "$OUT"

echo "Built: $OUT"
dpkg-deb -I "$OUT"
