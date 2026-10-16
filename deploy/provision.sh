#!/usr/bin/env bash
# One-time VPS setup for Free Direct Dials (Debian 12 / Ubuntu 22.04+), idempotent.
# Run as root:  ADMIN_IPS="203.0.113.7 198.51.100.0/24" bash provision.sh
#   ADMIN_IPS   (required) space-separated IPs/CIDRs allowed to open the superadmin panel
#   SSH_FROM    (optional) restrict SSH to these CIDRs (space-separated); default: anywhere
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run as root"; exit 1; }
: "${ADMIN_IPS:?set ADMIN_IPS to the IPs/CIDRs allowed to open the superadmin panel}"
SSH_FROM="${SSH_FROM:-}"
export DEBIAN_FRONTEND=noninteractive

echo "== packages"
apt-get update -q
apt-get install -yq --no-install-recommends ca-certificates curl gnupg ufw fail2ban unattended-upgrades \
  sqlite3 zstd systemd-zram-generator debian-keyring debian-archive-keyring apt-transport-https || \
apt-get install -yq --no-install-recommends ca-certificates curl gnupg ufw fail2ban unattended-upgrades sqlite3 zstd zram-tools
if ! command -v caddy >/dev/null; then
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -q && apt-get install -yq caddy
fi
dpkg-reconfigure -f noninteractive unattended-upgrades

echo "== user and directories"
id dials >/dev/null 2>&1 || useradd --system --home /var/lib/dials --shell /usr/sbin/nologin dials
install -d -o dials -g dials -m 0700 /var/lib/dials /var/lib/dials/tmp /var/backups/dials
install -d -o root -g root -m 0755 /opt/dials /opt/dials/releases
install -d -o root -g dials -m 0750 /etc/dials
[[ -f /etc/dials/dials.env ]] || { install -o root -g dials -m 0640 /dev/null /etc/dials/dials.env; echo "!! fill in /etc/dials/dials.env (see dials.env.example)"; }
chmod 0640 /etc/dials/dials.env && chown root:dials /etc/dials/dials.env

echo "== memory safety net: 1 GB swap + zram, low swappiness"
if [[ ! -f /swapfile ]]; then
  fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
  grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw,pri=10 0 0' >> /etc/fstab
fi
if [[ -d /etc/systemd/zram-generator.conf.d || -f /usr/lib/systemd/system-generators/zram-generator ]]; then
  printf '[zram0]\nzram-size = ram / 2\ncompression-algorithm = zstd\nswap-priority = 100\n' > /etc/systemd/zram-generator.conf
fi
cat > /etc/sysctl.d/90-dials.conf <<'EOF'
vm.swappiness = 20
vm.vfs_cache_pressure = 50
vm.dirty_background_ratio = 5
vm.dirty_ratio = 15
net.core.somaxconn = 4096
net.ipv4.tcp_fin_timeout = 15
net.ipv4.tcp_syncookies = 1
EOF
sysctl --system >/dev/null

echo "== logs capped at 100 MB"
mkdir -p /etc/systemd/journald.conf.d
printf '[Journal]\nSystemMaxUse=100M\nMaxRetentionSec=14day\n' > /etc/systemd/journald.conf.d/90-dials.conf
systemctl restart systemd-journald
install -d -o caddy -g caddy -m 0750 /var/log/caddy 2>/dev/null || true

echo "== Cloudflare ranges (origin only accepts Cloudflare on 443)"
cat > /usr/local/sbin/dials-cf-refresh <<'EOF'
#!/usr/bin/env bash
# Refresh Cloudflare IP ranges for ufw + Caddy trusted_proxies. Safe to run any time.
set -euo pipefail
v4=$(curl -fsS https://www.cloudflare.com/ips-v4) && v6=$(curl -fsS https://www.cloudflare.com/ips-v6)
[[ $(wc -l <<<"$v4") -gt 5 ]] || { echo "refusing: suspicious CF list"; exit 1; }
for r in $(ufw status numbered | awk '/443\/tcp.*cloudflare/ {gsub(/[\[\]]/,"",$1); print $1}' | sort -rn); do yes | ufw delete "$r" >/dev/null; done
for n in $v4 $v6; do ufw allow proto tcp from "$n" to any port 443 comment cloudflare >/dev/null; done
ips="$(echo $v4 $v6)"
sed -i "/^CLOUDFLARE_IPS=/d" /etc/caddy/caddy.env 2>/dev/null || true
echo "CLOUDFLARE_IPS=$ips" >> /etc/caddy/caddy.env
systemctl reload caddy 2>/dev/null || true
EOF
chmod 0755 /usr/local/sbin/dials-cf-refresh

echo "== firewall"
ufw --force reset >/dev/null
ufw default deny incoming && ufw default allow outgoing
if [[ -n "$SSH_FROM" ]]; then for c in $SSH_FROM; do ufw allow proto tcp from "$c" to any port 22 comment ssh; done
else ufw limit 22/tcp comment ssh; fi
touch /etc/caddy/caddy.env && chmod 0640 /etc/caddy/caddy.env && chown root:caddy /etc/caddy/caddy.env
/usr/local/sbin/dials-cf-refresh
ufw --force enable

echo "== Caddy environment"
ADMIN_PATH=$(grep -E '^ADMIN_PATH=' /etc/dials/dials.env | cut -d= -f2- || true)
sed -i '/^ADMIN_PATH=/d;/^ADMIN_ALLOW_CADDY=/d' /etc/caddy/caddy.env
echo "ADMIN_PATH=${ADMIN_PATH:-unset-admin-path}" >> /etc/caddy/caddy.env
echo "ADMIN_ALLOW_CADDY=$ADMIN_IPS" >> /etc/caddy/caddy.env
mkdir -p /etc/systemd/system/caddy.service.d
printf '[Service]\nEnvironmentFile=/etc/caddy/caddy.env\n' > /etc/systemd/system/caddy.service.d/env.conf
[[ -f /etc/caddy/origin.pem ]] || echo "!! put the Cloudflare Origin CA certificate at /etc/caddy/origin.pem and key at /etc/caddy/origin-key.pem (0640 root:caddy)"

echo "== fail2ban (ssh)"
printf '[sshd]\nenabled = true\nmaxretry = 5\nbantime = 1h\nfindtime = 10m\n' > /etc/fail2ban/jail.d/sshd.local
systemctl enable --now fail2ban >/dev/null

echo "== ssh: keys only (only if a key is already installed, so you can't lock yourself out)"
if compgen -G "/root/.ssh/authorized_keys" >/dev/null || compgen -G "/home/*/.ssh/authorized_keys" >/dev/null; then
  printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n' > /etc/ssh/sshd_config.d/90-dials.conf
  systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
else
  echo "!! no SSH key found: password login left ON. Add a key, then re-run this script."
fi

echo "== timers"
cat > /etc/systemd/system/dials-cf-refresh.service <<'EOF'
[Unit]
Description=Refresh Cloudflare IP ranges
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/dials-cf-refresh
EOF
cat > /etc/systemd/system/dials-cf-refresh.timer <<'EOF'
[Unit]
Description=Weekly Cloudflare IP refresh
[Timer]
OnCalendar=weekly
RandomizedDelaySec=6h
Persistent=true
[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload
systemctl enable --now dials-cf-refresh.timer fstrim.timer >/dev/null
[[ -f /etc/systemd/system/dials.service ]] && systemctl enable dials >/dev/null || true

echo
echo "Done. Next: copy deploy/dials.service to /etc/systemd/system/, deploy/Caddyfile to /etc/caddy/,"
echo "fill /etc/dials/dials.env, add the Origin CA cert, then run deploy/deploy.sh from your machine."
