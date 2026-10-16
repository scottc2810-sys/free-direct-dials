#!/usr/bin/env bash
# Free Direct Dials: install on a fresh Ubuntu 22.04/24.04 or Debian 12 VM from an
# extracted release bundle. Safe to re-run: an existing /etc/dials/dials.env, the
# database and backups are kept; blanks are filled; the site starts as soon as
# everything it needs is in place.
#
#   sudo ADMIN_IPS="203.0.113.7" bash deploy/install.sh
#
# Required:  ADMIN_IPS       your public IP(s)/CIDRs allowed to open the superadmin panel
# Optional:  PUBLIC_ORIGIN   default https://markemindai.com
#            BASE_PATH       default /free-direct-dials
#            SSH_FROM        restrict SSH to these CIDRs (default: anywhere, rate-limited)
#            TURNSTILE_SITE_KEY TURNSTILE_SECRET BREVO_API_KEY RESEND_API_KEY ADMIN_EMAIL MAIL_FROM
#                            pre-fill these instead of editing /etc/dials/dials.env afterwards
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run it with sudo:  sudo ADMIN_IPS=\"your.ip\" bash deploy/install.sh"; exit 1; }
: "${ADMIN_IPS:?set ADMIN_IPS to your public IP (Google \"what is my ip\"), e.g. ADMIN_IPS=\"203.0.113.7\"}"
HERE=$(cd "$(dirname "$0")/.." && pwd)
VER=$(cat "$HERE/VERSION" 2>/dev/null || echo manual)
ENVF=/etc/dials/dials.env

case "$(uname -m)" in x86_64) want=amd64 ;; aarch64 | arm64) want=arm64 ;; *) want="$(uname -m)" ;; esac
have=$(cat "$HERE/ARCH" 2>/dev/null || echo "$want")
[[ "$have" == "$want" ]] || { echo "!! this bundle is linux-$have but this VM is $(uname -m): download the linux-$want bundle"; exit 1; }
[[ -f "$HERE/dials" ]] || { echo "!! no dials binary in $HERE (run this from the extracted bundle)"; exit 1; }

say() { printf '\n== %s\n' "$*"; }
envget() { grep -E "^$1=" "$ENVF" 2>/dev/null | head -1 | cut -d= -f2- || true; }
envset() { # key value: replace the line, or append it
  if grep -qE "^$1=" "$ENVF"; then
    awk -v k="$1" -v v="$2" 'BEGIN { FS = OFS = "=" } $1 == k { print k "=" v; next } { print }' "$ENVF" >"$ENVF.tmp" && cat "$ENVF.tmp" >"$ENVF" && rm -f "$ENVF.tmp"
  else
    printf '%s=%s\n' "$1" "$2" >>"$ENVF"
  fi
}
rand() { if command -v openssl >/dev/null; then openssl rand "$@"; else head -c 48 /dev/urandom | base64 | tr -d '\n'; fi; }

say "configuration: $ENVF"
install -d -m 0750 /etc/dials
if [[ ! -s "$ENVF" ]]; then
  install -m 0600 "$HERE/deploy/dials.env.example" "$ENVF"
  echo "created from dials.env.example"
fi
[[ -n "$(envget SECRET_KEY)" ]] || envset SECRET_KEY "$(rand -base64 48 | tr -d '\n')"
[[ -n "$(envget ADMIN_PATH)" ]] || envset ADMIN_PATH "$(rand -hex 16)"
[[ -n "$(envget BACKUP_KEY)" ]] || envset BACKUP_KEY "$(rand -hex 32)"
[[ -n "$(envget MAIL_WEBHOOK_TOKEN)" ]] || envset MAIL_WEBHOOK_TOKEN "$(rand -hex 32)"
envset ADMIN_ALLOW_CIDRS "$(echo $ADMIN_IPS | tr ' ' ',')"
[[ -z "${PUBLIC_ORIGIN:-}" ]] || envset PUBLIC_ORIGIN "$PUBLIC_ORIGIN"
[[ -z "${BASE_PATH:-}" ]] || envset BASE_PATH "$BASE_PATH"
for k in TURNSTILE_SITE_KEY TURNSTILE_SECRET BREVO_API_KEY RESEND_API_KEY ADMIN_EMAIL MAIL_FROM; do
  [[ -z "${!k:-}" ]] || envset "$k" "${!k}"
done
# the example lists brevo and resend; keep only providers that have a key
prov=$(envget MAIL_PROVIDERS)
if [[ "$prov" == "brevo:300,resend:100" ]]; then
  p=""
  [[ -n "$(envget BREVO_API_KEY)" ]] && p="brevo:300"
  [[ -n "$(envget RESEND_API_KEY)" ]] && p="${p:+$p,}resend:100"
  [[ -n "$p" ]] && envset MAIL_PROVIDERS "$p"
fi

say "server setup (packages, Caddy, firewall, swap, fail2ban, timers)"
FROM_INSTALLER=1 ADMIN_IPS="$ADMIN_IPS" SSH_FROM="${SSH_FROM:-}" bash "$HERE/deploy/provision.sh"
chown root:dials "$ENVF" && chmod 0640 "$ENVF"

say "service files"
install -m 0644 "$HERE/deploy/dials.service" /etc/systemd/system/dials.service
if [[ -f /etc/caddy/Caddyfile ]] && ! cmp -s "$HERE/deploy/Caddyfile" /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%Y%m%d%H%M%S)"
fi
install -m 0644 "$HERE/deploy/Caddyfile" /etc/caddy/Caddyfile
systemctl daemon-reload
systemctl enable dials caddy >/dev/null 2>&1 || true

say "release $VER"
install -d -m 0755 "/opt/dials/releases/$VER"
install -m 0755 "$HERE/dials" "/opt/dials/releases/$VER/dials"
ln -sfn "/opt/dials/releases/$VER" /opt/dials/current.new && mv -T /opt/dials/current.new /opt/dials/current
for f in DEPLOY.md RUNBOOK.md MONETIZATION.md; do [[ -f "$HERE/docs/$f" ]] && install -m 0644 "$HERE/docs/$f" "/opt/dials/$f"; done
cat >/usr/local/bin/dials <<'EOF'
#!/bin/sh
# Run a Free Direct Dials command as the service user with the production config.
exec sudo -u dials env DIALS_ENV_FILE=/etc/dials/dials.env /opt/dials/current/dials "$@"
EOF
chmod 0755 /usr/local/bin/dials

# Cloudflare Origin CA certificate (pasted by hand): fix ownership if present
for f in /etc/caddy/origin.pem /etc/caddy/origin-key.pem; do
  [[ -f "$f" ]] && chown root:caddy "$f" && chmod 0640 "$f"
done

say "what is still needed"
missing=()
[[ -n "$(envget TURNSTILE_SITE_KEY)" && -n "$(envget TURNSTILE_SECRET)" ]] || missing+=("Turnstile keys: TURNSTILE_SITE_KEY and TURNSTILE_SECRET in $ENVF")
mp=$(envget MAIL_PROVIDERS)
[[ "$mp" != *brevo* || -n "$(envget BREVO_API_KEY)" ]] || missing+=("BREVO_API_KEY in $ENVF (or remove brevo from MAIL_PROVIDERS)")
[[ "$mp" != *resend* || -n "$(envget RESEND_API_KEY)" ]] || missing+=("RESEND_API_KEY in $ENVF (or remove resend from MAIL_PROVIDERS)")
[[ -s /etc/caddy/origin.pem && -s /etc/caddy/origin-key.pem ]] || missing+=("Cloudflare Origin certificate: /etc/caddy/origin.pem and /etc/caddy/origin-key.pem")
base=$(envget BASE_PATH); base=${base:-/free-direct-dials}
origin=$(envget PUBLIC_ORIGIN)

if ((${#missing[@]})); then
  echo "Installed. Before the site can start, add:"
  for m in "${missing[@]}"; do echo "  - $m"; done
  echo
  echo "Edit the config:   sudo nano $ENVF"
  echo "Paste the cert:    sudo nano /etc/caddy/origin.pem   and   sudo nano /etc/caddy/origin-key.pem"
  echo "Then run the same install command again; it finishes and starts the site."
  exit 0
fi

say "config check"
if ! /usr/local/bin/dials check; then
  echo "!! the configuration has errors (above). Fix them with: sudo nano $ENVF   then re-run this installer."
  exit 1
fi

say "start"
systemctl restart dials
ok=""
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:8081${base}/readyz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
[[ -n "$ok" ]] || { echo "!! the app did not become ready:"; journalctl -u dials -n 40 --no-pager; exit 1; }
if ! systemctl restart caddy; then
  echo "!! Caddy failed to start (usually the Origin certificate): journalctl -u caddy -n 30"
  exit 1
fi

cat <<EOF

Free Direct Dials $VER is running.
  Site:         ${origin}${base}/
  Superadmin:   ${origin}${base}/$(envget ADMIN_PATH)/   (only from: $ADMIN_IPS)
  Health:       curl -s http://127.0.0.1:8081${base}/readyz

Next:
  1. Create your superadmin login:   sudo dials admin create yourname
  2. SAVE this backup key in your password manager (backups can't be restored without it):
       $(envget BACKUP_KEY)
  3. Follow docs/DEPLOY.md, step 9 onwards (first settings, data upload).
EOF
