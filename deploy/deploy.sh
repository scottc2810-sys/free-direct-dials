#!/usr/bin/env bash
# Build, test, ship and switch to a new release with a health gate and automatic rollback.
#   deploy/deploy.sh user@your-vps            (needs sudo on the VPS)
#   SKIP_TESTS=1 deploy/deploy.sh user@host   (not recommended)
set -euo pipefail
HOST=${1:?usage: deploy.sh user@host}
cd "$(dirname "$0")/.."
VER=$(git describe --always --dirty 2>/dev/null || date -u +%Y%m%d%H%M%S)
VER=${VER//\//-}

if [[ -z "${SKIP_TESTS:-}" ]]; then
  echo "== tests"
  go vet ./...
  go test ./... -count=1
fi
echo "== build linux/amd64 $VER"
mkdir -p build
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VER" -o build/dials ./cmd/dials
sha=$(sha256sum build/dials | cut -c1-16)

echo "== upload"
scp -q build/dials "$HOST:/tmp/dials-$VER"
scp -q docs/RUNBOOK.md "$HOST:/tmp/RUNBOOK.md" 2>/dev/null || true

echo "== switch (backup → swap → restart → readyz, rollback on failure)"
ssh "$HOST" VER="$VER" SHA="$sha" 'bash -s' <<'REMOTE'
set -euo pipefail
sudo install -o root -g root -m 0755 "/tmp/dials-$VER" "/opt/dials/releases/dials-$VER"
[[ -f /tmp/RUNBOOK.md ]] && sudo install -m 0644 /tmp/RUNBOOK.md /opt/dials/RUNBOOK.md || true
sudo mkdir -p "/opt/dials/releases/$VER" && sudo mv "/opt/dials/releases/dials-$VER" "/opt/dials/releases/$VER/dials"
prev=$(readlink -f /opt/dials/current 2>/dev/null || true)
if [[ -n "$prev" && -f /var/lib/dials/dials.db ]]; then
  echo "pre-deploy backup…"
  sudo -u dials env DIALS_ENV_FILE=/etc/dials/dials.env "$prev/dials" backup || echo "!! backup failed (continuing; check disk)"
fi
sudo ln -sfn "/opt/dials/releases/$VER" /opt/dials/current.new && sudo mv -T /opt/dials/current.new /opt/dials/current
sudo systemctl restart dials
base=$(grep -E '^BASE_PATH=' /etc/dials/dials.env | cut -d= -f2- || echo /free-direct-dials)
for i in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:8081${base}/readyz" >/dev/null 2>&1; then
    echo "healthy: $VER ($SHA)"
    ls -1dt /opt/dials/releases/*/ | tail -n +6 | xargs -r sudo rm -rf   # keep 5 releases
    exit 0
  fi
  sleep 1
done
echo "!! readyz failed: rolling back to $prev"
sudo journalctl -u dials -n 40 --no-pager || true
if [[ -n "$prev" ]]; then
  sudo ln -sfn "$prev" /opt/dials/current.new && sudo mv -T /opt/dials/current.new /opt/dials/current
  sudo systemctl restart dials
fi
exit 1
REMOTE
