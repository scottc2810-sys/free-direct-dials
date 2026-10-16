#!/usr/bin/env bash
# Free Direct Dials: switch to the release in this extracted bundle.
# backup → swap → restart → health check → automatic rollback if it isn't healthy.
#
#   sudo bash deploy/upgrade.sh
#
# Database migrations run on start and only ever add tables/columns, so the
# previous release still runs if a rollback happens.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run it with sudo:  sudo bash deploy/upgrade.sh"; exit 1; }
HERE=$(cd "$(dirname "$0")/.." && pwd)
VER=$(cat "$HERE/VERSION" 2>/dev/null || date -u +%Y%m%d%H%M%S)
ENVF=/etc/dials/dials.env

case "$(uname -m)" in x86_64) want=amd64 ;; aarch64 | arm64) want=arm64 ;; *) want="$(uname -m)" ;; esac
have=$(cat "$HERE/ARCH" 2>/dev/null || echo "$want")
[[ "$have" == "$want" ]] || { echo "!! this bundle is linux-$have but this VM is $(uname -m)"; exit 1; }
[[ -x /opt/dials/current/dials ]] || { echo "!! not installed yet: run deploy/install.sh first"; exit 1; }
prev=$(readlink -f /opt/dials/current)
if [[ "$prev" == "/opt/dials/releases/$VER" && -z "${FORCE:-}" ]]; then
  echo "already running $VER (FORCE=1 to reinstall it)"
  exit 0
fi

echo "== install $VER"
install -d -m 0755 "/opt/dials/releases/$VER"
install -m 0755 "$HERE/dials" "/opt/dials/releases/$VER/dials"
for f in DEPLOY.md RUNBOOK.md MONETIZATION.md; do [[ -f "$HERE/docs/$f" ]] && install -m 0644 "$HERE/docs/$f" "/opt/dials/$f"; done
if ! cmp -s "$HERE/deploy/dials.service" /etc/systemd/system/dials.service; then
  install -m 0644 "$HERE/deploy/dials.service" /etc/systemd/system/dials.service
  systemctl daemon-reload
  echo "service file updated"
fi
caddy_changed=""
if ! cmp -s "$HERE/deploy/Caddyfile" /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%Y%m%d%H%M%S)"
  install -m 0644 "$HERE/deploy/Caddyfile" /etc/caddy/Caddyfile
  caddy_changed=1
  echo "Caddyfile updated (old one backed up)"
fi

if [[ -f /var/lib/dials/dials.db ]]; then
  echo "== pre-upgrade backup"
  sudo -u dials env DIALS_ENV_FILE="$ENVF" "$prev/dials" backup || echo "!! backup failed (continuing; check disk space)"
fi

echo "== switch $(basename "$prev") → $VER"
ln -sfn "/opt/dials/releases/$VER" /opt/dials/current.new && mv -T /opt/dials/current.new /opt/dials/current
systemctl restart dials
base=$(grep -E '^BASE_PATH=' "$ENVF" | cut -d= -f2- || true)
base=${base:-/free-direct-dials}
for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:8081${base}/readyz" >/dev/null 2>&1; then
    [[ -z "$caddy_changed" ]] || systemctl reload caddy || systemctl restart caddy
    ls -1dt /opt/dials/releases/*/ | tail -n +6 | xargs -r rm -rf # keep the 5 newest
    echo "healthy: $VER"
    exit 0
  fi
  sleep 1
done

echo "!! $VER did not become healthy; rolling back to $(basename "$prev")"
journalctl -u dials -n 40 --no-pager || true
ln -sfn "$prev" /opt/dials/current.new && mv -T /opt/dials/current.new /opt/dials/current
systemctl restart dials
exit 1
