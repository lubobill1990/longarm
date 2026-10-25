#!/bin/sh
# Longarm prerm: on removal (not upgrade), stop the longarmd instances and leave
# the hub (revokes the worker there, removes its credentials).
set -e
case "$1" in
  remove|purge|0) ;;
  *) exit 0 ;;
esac
command -v systemctl >/dev/null 2>&1 || exit 0
for unit in $(systemctl list-units --all --plain --no-legend 'longarmd@*.service' 2>/dev/null | awk '{print $1}'); do
  user="${unit#longarmd@}"; user="${user%.service}"
  systemctl disable --now "$unit" || true
  su -s /bin/sh "$user" -c "longarmd leave --kill-jobs" || true
done
exit 0
