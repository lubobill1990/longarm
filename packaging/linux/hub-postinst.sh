#!/bin/sh
set -e
[ "$1" = "configure" ] || [ "$1" = "1" ] || [ "$1" = "2" ] || exit 0
getent passwd longarm-hub >/dev/null || useradd --system --home /var/lib/longarm-hub --shell /usr/sbin/nologin longarm-hub
install -d -m 0750 -o root -g longarm-hub /etc/longarm-hub
if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload || true; fi
echo "longarm-hub installed: cp /etc/longarm-hub/hub.toml.example /etc/longarm-hub/hub.toml, edit, then systemctl enable --now longarm-hub"
exit 0
