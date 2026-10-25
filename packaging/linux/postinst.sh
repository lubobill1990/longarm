#!/bin/sh
# Longarm postinst: optional join + service (see /etc/longarm/join.env.example).
set -e
[ "$1" = "configure" ] || [ "$1" = "1" ] || [ "$1" = "2" ] || exit 0

if [ -f /etc/longarm/join.env ]; then
  # shellcheck disable=SC1091
  . /etc/longarm/join.env
fi
ROLES="${LONGARM_ROLES:-}"
HUB="${LONGARM_HUB:-}"
CODE="${LONGARM_JOIN_CODE:-}"
FP="${LONGARM_CA_FINGERPRINT:-}"
USR="${LONGARM_USER:-}"

if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload || true; fi

if [ -z "$ROLES" ] || [ -z "$HUB" ] || [ -z "$CODE" ] || [ -z "$USR" ]; then
  echo "Longarm installed. Join a hub with:"
  echo "  worker: sudo -u <user> longarmd join <hub> <code> && sudo systemctl enable --now longarmd@<user>"
  echo "  client: longarm join <hub> <code>"
  exit 0
fi
fp_arg=""
[ -n "$FP" ] && fp_arg="--ca-fingerprint $FP"
case "$ROLES" in
  worker|both)
    # shellcheck disable=SC2086
    su -s /bin/sh "$USR" -c "longarmd join '$HUB' '$CODE' --no-wait $fp_arg" || { echo "Longarm: longarmd join failed" >&2; exit 0; }
    systemctl enable --now "longarmd@$USR" || true
    ;;
  client)
    # shellcheck disable=SC2086
    su -s /bin/sh "$USR" -c "longarm join '$HUB' '$CODE' $fp_arg" || echo "Longarm: longarm join failed" >&2
    ;;
esac
[ -f /etc/longarm/join.env ] && mv /etc/longarm/join.env /etc/longarm/join.env.done
exit 0
