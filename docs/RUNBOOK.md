# Free Direct Dials — Operations Runbook

> For whoever is on call. Commands assume the VPS layout created by `deploy/provision.sh`:
> binary `/opt/dials/current/dials`, data `/var/lib/dials`, backups `/var/backups/dials`, config `/etc/dials/dials.env`.
> Run CLI commands as the service user: `sudo -u dials env DIALS_ENV_FILE=/etc/dials/dials.env /opt/dials/current/dials <cmd>`
> (shortened below to `dials <cmd>`).

## 1. First-time setup (about 1 hour)

### 1.1 Cloudflare (domain `markemindai.com` must use Cloudflare DNS)
1. **DNS:** add `A origin-dials → <VPS IP>`, **Proxied** (orange cloud).
2. **SSL/TLS → Origin Server → Create certificate** for `markemindai.com, *.markemindai.com` (15 years). Save it as `/etc/caddy/origin.pem` and the key as `/etc/caddy/origin-key.pem` (`chown root:caddy`, `chmod 0640`).
3. **SSL/TLS mode:** *Full (strict)*. If the main site's host can't do strict, add a **Configuration Rule** for `URI Path starts with /free-direct-dials` → SSL *Full (strict)* and leave the zone default as it is.
4. **Rules → Origin Rules → Create:** when `URI Path starts with "/free-direct-dials"` **or** `starts with "/free-direct-dails"` → *DNS record override* = `origin-dials.markemindai.com`. The main site keeps everything else.
5. **Rules → Redirect Rules (Single):** `/free-direct-dails*` → `https://markemindai.com/free-direct-dials${1}` (301). The app also redirects, so this one is belt and braces.
6. **Caching → Cache Rules** (in this order):
   - `/free-direct-dials/assets/` → *Eligible for cache*, Edge TTL **1 year**.
   - `/free-direct-dials/api/me`, `/api/ad/`, `/api/auth/`, `/api/remove/`, `/api/unsub`, `/api/hooks/`, and the admin path → **Bypass cache**.
   - `/free-direct-dials/` (everything else) → *Eligible for cache*, Edge TTL **"Use cache-control header from origin"**. The origin already sends `s-maxage` (60 s for API, 600 s for pages).
   - Cache key: keep the query string (the default).
7. **Security → WAF → Custom rules** (layer 1 of the superadmin allow-list):
   `(http.request.uri.path contains "/free-direct-dials/<ADMIN_PATH>") and not (ip.src in {203.0.113.7 198.51.100.0/24})` → **Block**.
   Use the same IPs as `ADMIN_ALLOW_CIDRS`.
8. **Security → WAF → Rate limiting rule** (the free plan includes one): `URI Path contains "/free-direct-dials/api/auth/"` → 20 requests / 10 s per IP → Block 1 minute.
9. **Turnstile:** add a widget for `markemindai.com`, mode *Managed*. Put the keys in `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET`.
10. Optional: an **API token** with only `Zone → Cache Purge` for this zone goes in `CF_API_TOKEN` and `CF_ZONE_ID`. It lets imports and settings saves show up instantly.

### 1.2 VPS
```bash
scp -r deploy root@VPS:/root/
ssh root@VPS 'ADMIN_IPS="203.0.113.7 198.51.100.0/24" bash /root/deploy/provision.sh'
ssh root@VPS 'cp /root/deploy/dials.service /etc/systemd/system/ && cp /root/deploy/Caddyfile /etc/caddy/ && systemctl daemon-reload'
ssh root@VPS 'cp /root/deploy/dials.env.example /etc/dials/dials.env && nano /etc/dials/dials.env'   # fill every blank
ssh root@VPS 'ADMIN_IPS="203.0.113.7 198.51.100.0/24" bash /root/deploy/provision.sh'   # re-run: picks up ADMIN_PATH for Caddy
```
Generate the secrets with `openssl rand -base64 48` (SECRET_KEY), `openssl rand -hex 16` (ADMIN_PATH), `openssl rand -hex 32` (BACKUP_KEY, MAIL_WEBHOOK_TOKEN). **Keep a copy of BACKUP_KEY in your password manager.** Without it, encrypted backups can't be restored.

### 1.3 Email (Brevo recommended: 300/day free)
1. Add `markemindai.com` as a sender domain. Publish the **SPF**, **DKIM** and **DMARC** records Brevo shows (`v=DMARC1; p=quarantine; rua=mailto:…`).
2. Put the API key in `BREVO_API_KEY`. Add Resend as the second provider for failover.
3. Webhooks: send *hard bounce, spam, blocked, unsubscribed* to `https://markemindai.com/free-direct-dials/api/hooks/mail/brevo?k=<MAIL_WEBHOOK_TOKEN>`.

### 1.4 First deploy and superadmin
```bash
deploy/deploy.sh admin@VPS                    # from your machine (Git Bash on Windows works)
ssh admin@VPS 'sudo systemctl start caddy'    # then visit https://markemindai.com/free-direct-dials/
ssh -t admin@VPS 'sudo -u dials env DIALS_ENV_FILE=/etc/dials/dials.env /opt/dials/current/dials admin create yourname'
```
Scan the QR with an authenticator app, then open `https://markemindai.com/free-direct-dials/<ADMIN_PATH>/` **from an allow-listed IP**. Work through the dashboard checklist: postal address, contact email, WhatsApp number, UPI ID, PayPal, Adsterra codes.

Submit `https://markemindai.com/free-direct-dials/sitemap.xml` in Google Search Console and Bing Webmaster Tools. If the main site serves `robots.txt`, add `Sitemap: https://markemindai.com/free-direct-dials/sitemap.xml` to it. You can also link `/free-direct-dials/llms.txt` from the root `llms.txt`.

### 1.5 ads.txt at the domain root
Ad networks read only `https://markemindai.com/ads.txt`. Either add the lines to the main site's own file, **or** let this server serve it:
Cloudflare → Rules → Origin Rules → `URI Path equals "/ads.txt"` → DNS override `origin-dials.markemindai.com`. The Caddyfile passes `/ads.txt` to the app, which serves Superadmin → Ads → ads.txt (empty = 404). Check with `curl -s https://markemindai.com/ads.txt`. Details: docs/MONETIZATION.md §4.

### 1.6 Extra contact databases (Supabase free projects)
The VPS database always holds accounts and settings, and is searched first. Supabase projects add room for contacts.
1. supabase.com → New project (pick the region closest to the VPS) → save the database password.
2. Project → **Connect** → **Session pooler** → copy the URI (IPv4; the direct host is IPv6-only on the free plan). Replace `[YOUR-PASSWORD]`.
3. Superadmin → **Databases** → name, quota **500 MB** → *Test connection* → *Add database*. The app creates its tables in schema `fdd` (not exposed by Supabase's REST/GraphQL APIs; row-level security on).
4. Uploads: *Store new contacts in* → **Auto** fills one database to 90 % then moves to the next. People already in any database merge where they are.
5. For certificate pinning, download Supabase's CA (Project Settings → Database → SSL) to `/etc/dials/supabase-ca.crt` and append `?sslmode=verify-full&sslrootcert=/etc/dials/supabase-ca.crt` to the connection string. Without it the connection is still encrypted (`sslmode=require` is enforced).
- Every database is pinged at startup and every **6 hours** (a real write), so free projects never hit the 7-day inactivity pause. `dials stores` lists them all with size, contacts and state.
- Supabase allows **two** free projects per account. Don't create extra accounts to get more (it breaks their terms); upgrade one project to Pro (8 GB) instead.
- Integration tests against a **throwaway** project: put `DIALS_TEST_PG_DSN=postgresql://…` in `data/test-supabase.env` (git-ignored) and run `go test ./internal/store/ -run Postgres`. The test drops and recreates schema `fdd` there.

## 2. Superadmin access (IP allow-list)

Three independent layers have to agree. If any one of them denies a request, the visitor gets a plain 404.

| Layer | Where | Change it by |
|---|---|---|
| 1 | Cloudflare WAF custom rule | Dashboard → Security → WAF |
| 2 | Caddy `@adminDenied` | `ADMIN_IPS="…" bash provision.sh` (rewrites `/etc/caddy/caddy.env`) |
| 3 | App `ADMIN_ALLOW_CIDRS` (required in prod) | edit `/etc/dials/dials.env`, then `sudo systemctl restart dials` (≈1 s) |

The app trusts the client IP only from Caddy on loopback, and Caddy trusts `CF-Connecting-IP` only from Cloudflare's ranges. Port 443 accepts only Cloudflare, so the IP can't be spoofed. **Locked out after your IP changed?** SSH in and update layers 3 and 2, then layer 1. Denied attempts are logged: `journalctl -u dials | grep "access denied"`.

## 3. Everyday tasks

| Task | How |
|---|---|
| Upload data | Superadmin → Upload: CSV / TSV / .csv.gz / **.xlsx**, up to 5 GB, resumable. Map columns → **Check mapping** (200-row dry run) → Import. |
| Huge file (> 1 GB) | `scp file.csv VPS:/var/lib/dials/tmp/` then `dials inspect file.csv` and `dials import file.csv`. |
| Check a vendor file | `dials inspect file.xlsx [-sheet 2]`: fill rate, distinct values, masked samples, auto-mapping. |
| Notify subscribers | Superadmin → Imports → **Notify subscribers**. Once per upload; sends within the daily quota and continues the next day. |
| Change ads / donations / WhatsApp | Superadmin → Settings. Live within 60 s (instantly with `CF_API_TOKEN`). |
| Removal request | Superadmin → Removals. Self-service email removals are instant; others are due within 30 days. |
| Maintenance window | Superadmin → Site → Maintenance mode. Search keeps working; sign-ups and unlocks pause. |

## 4. Backups and restore

- **Automatic:** daily at 03:30 UTC. The job runs `VACUUM INTO`, an integrity check, zstd compression, then AES-256-GCM encryption when `BACKUP_KEY` is set. It keeps the 2 newest in `/var/backups/dials` and is skipped (with an alert) if free disk is below 1.3 × DB + 1 GB.
- **Now:** `dials backup` (also in Superadmin → System).
- **Offsite:** strongly recommended. Install `rclone` and configure Cloudflare R2 or Backblaze B2 (free tiers), then add a timer: `rclone copy /var/backups/dials r2:dials-backups --max-age 26h`. The files are encrypted, so the bucket never holds readable personal data.
- **Restore** (about 5 min):
  ```bash
  sudo systemctl stop dials
  sudo -u dials mv /var/lib/dials/dials.db /var/lib/dials/dials.db.broken; sudo -u dials rm -f /var/lib/dials/dials.db-wal /var/lib/dials/dials.db-shm
  sudo -u dials env DIALS_ENV_FILE=/etc/dials/dials.env /opt/dials/current/dials restore /var/backups/dials/dials-YYYYMMDD-HHMMSS.db.zst.enc /var/lib/dials/dials.db
  sudo systemctl start dials && curl -s localhost:8081/free-direct-dials/readyz
  ```

## 5. Alerts → what to do

| Alert / symptom | Meaning | Action |
|---|---|---|
| **Disk 80 %** | early warning | Delete stale `tmp/upload-*` (automatic after 2 h). `journalctl --vacuum-size=50M`. Check the backup sizes. |
| **Disk 90 %** | uploads, imports and backups are paused automatically | Free space now: old releases are under `/opt/dials/releases`. If the free-list is large, run `VACUUM` with `dials` stopped. If the DB is over 6 GB, plan a bigger disk. |
| **Disk 95 %** | the site is read-only: no sign-ups or unlocks | Same as above, urgently. Browsing still works. |
| **Email quota exhausted** | every provider hit its cap | Add or raise a provider in `MAIL_PROVIDERS`, or wait for UTC midnight. Sign-in codes use the reserved share first. |
| **Sign-in email failed** | provider error (bad key? suspended?) | Superadmin → Email → *Send test email*. Check the provider dashboard. |
| **Background job failed: backup** | see the message (usually disk) | Fix, then Superadmin → System → *Back up now*. |
| **Import failed** | message in Superadmin → Imports | Re-run the upload. Re-importing never duplicates rows. |
| 503 "busy" on searches | more than 8 concurrent uncached queries | Usually a crawler. Cloudflare → Security → *Under Attack mode* for an hour. Check the Cache Rules. |
| p95 above 500 ms on searches | DB outgrew RAM | `dials bench` and compare with docs/PERFORMANCE.md. Move to 2 GB RAM (no code changes). |
| Ad account warning from Adsterra | policy flag | Superadmin → Ads → keep *Sponsor break* mode and don't incentivise clicks. |
| **Contact database unreachable** | email alert; dashboard warning; that store shows *down* | Visitors still get every other database (results marked partial) and uploads pause. A paused Supabase project: open it in the dashboard → *Restore*. Then Superadmin → Databases → *Ping all now*. |
| **Contact database N % full** | email alert at 80 %+ | At 90 % Auto uploads move to the next database. Add a database, or raise the quota after upgrading the project. |

## 6. Health checks

- `GET /free-direct-dials/healthz`: process alive (use it for the uptime monitor every minute, e.g. UptimeRobot or Better Stack free).
- `GET /free-direct-dials/readyz`: DB readable and writable plus disk level (returns 503 at ≥ 95 %).
- `dials check`: configuration summary and whether a superadmin exists.
- `journalctl -u dials -f`: the app logs only warnings and errors in prod. Request IDs match the `X-Request-ID` header.

## 7. Scaling path (no rewrite needed)

| Signal | Step |
|---|---|
| more than 2 M contacts or DB over 6 GB | VPS with 2 GB RAM and 40 GB disk: copy `/var/lib/dials`, done |
| sustained > 50 uncached req/s | 2 vCPU. Raise `Readers` in `db.Options` |
| contacts outgrow the VPS disk | add Supabase/Postgres databases in Superadmin → Databases (§1.6); the VPS keeps accounts and stays first in search order |
| multiple app servers needed | the control plane (users, sessions, settings) is still SQLite: move it to Postgres behind the same interfaces |
