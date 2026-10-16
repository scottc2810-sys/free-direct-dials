# Deploy Free Direct Dials on a VM with PuTTY (Windows), step by step

You end up with the site live at `https://markemindai.com/free-direct-dials/` on your own VM, behind Cloudflare, with the superadmin panel reachable only from your IP.

- **Time:** about an hour, most of it waiting for DNS and copying keys.
- **Typing:** every command below is meant to be pasted into PuTTY as written. Only change the parts in `ANGLE_BRACKETS` or marked *yours*.
- **Bundle:** `free-direct-dials-<version>-linux-amd64.tar.gz`, or `-linux-arm64` for ARM VMs. It holds the server, the installer, the upgrade script and these docs. Download it from the [GitHub releases](https://github.com/scottc2810-sys/free-direct-dials/releases).

---

## 0. What you need

| Item | Where | Notes |
|---|---|---|
| A VM | Any cloud: Oracle Cloud, DigitalOcean, Hetzner, AWS Lightsail, Vultr… | **Ubuntu 24.04 LTS** (22.04 and Debian 12 also work). 1 vCPU, **1 GB RAM** recommended (0.75 GB is the floor), **20 GB disk** or more. |
| The domain on Cloudflare | dash.cloudflare.com | `markemindai.com` must use Cloudflare DNS (the free plan is enough). |
| Your public IP | Google "what is my ip" | This is the only IP that can open the superadmin panel (several can be listed). |
| Email sending | brevo.com (free: 300/day) | An API key, and later the sender-domain DNS records. |
| PuTTY | Your Windows PC | Step 1. |

## 1. Install PuTTY and make an SSH key (Windows)

1. Install PuTTY. Either download the 64-bit MSI from the official page, https://www.chiark.greenend.org.uk/~sgtatham/putty/latest.html, or run this in Windows PowerShell:
   ```powershell
   winget install --id PuTTY.PuTTY -e
   ```
2. Start **PuTTYgen** (Start menu → PuTTY → PuTTYgen).
3. *Key* menu → **EdDSA**, curve **Ed25519**. Click **Generate** and move the mouse over the blank area until the bar fills.
4. Set *Key comment* to `dials-vm` and enter a **Key passphrase**, twice.
5. Click **Save private key** and save it as `C:\keys\dials-vm.ppk`. Keep this file private.
6. Select **all** the text in the box *"Public key for pasting into OpenSSH authorized_keys file"* (it starts with `ssh-ed25519`) and copy it. You paste it into the cloud console in step 2.

## 2. Create the VM

In your cloud provider's console:

1. **Image:** Ubuntu 24.04 LTS (x86/AMD = `amd64` bundle; ARM/Ampere/Graviton = `arm64` bundle).
2. **Size:** 1 vCPU / 1 GB RAM / 20 GB disk or larger.
3. **SSH key:** paste the public key from step 1.6.
4. **Firewall / security group / security list:** allow inbound **TCP 22** (SSH) and **TCP 443** (HTTPS). The server's own firewall later restricts 443 to Cloudflare.
5. Create it, then note the **public IP** (below: `<VM_IP>`).

The login user depends on the image: Ubuntu → `ubuntu`, Debian → `debian` (AWS Debian: `admin`), DigitalOcean/Hetzner/Vultr → `root`. Below: `<USER>`.

> **Oracle Cloud only:**
> - Also add an *Ingress Rule* for TCP 443 in the VCN's *Security List*.
> - Oracle's Ubuntu images ship their own iptables rules that block 443, even after ufw allows it. Once you're connected (step 3), run:
>   ```bash
>   sudo apt-get purge -y netfilter-persistent iptables-persistent && sudo reboot
>   ```
>   Wait a minute, then reconnect.

## 3. Connect with PuTTY

1. Open **PuTTY**.
2. **Session:** *Host Name* = `<USER>@<VM_IP>` (e.g. `ubuntu@203.0.113.50`), *Port* = `22`, *Connection type* = SSH.
3. **Connection → SSH → Auth → Credentials:** *Private key file for authentication* → **Browse** → `C:\keys\dials-vm.ppk`.
4. **Connection:** *Seconds between keepalives* = `30`, so idle sessions don't drop.
5. **Window → Translation:** *Remote character set* = **UTF-8**. The superadmin QR code needs it.
6. Back to **Session:** *Saved Sessions* = `dials` → **Save**. Next time, double-click `dials`.
7. Click **Open**. On first connect PuTTY asks about the host key: click **Accept**. Enter your key passphrase.

PuTTY tips: **right-click pastes**. Selecting text with the mouse copies it. `Ctrl+C` stops a running command; it doesn't copy.

Become root for the rest of the guide:
```bash
sudo -i
```

## 4. Download the release onto the VM

```bash
cd /root
VER=v1.0.0
ARCH=$(dpkg --print-architecture)      # prints amd64 or arm64
curl -fLO https://github.com/scottc2810-sys/free-direct-dials/releases/download/$VER/free-direct-dials-$VER-linux-$ARCH.tar.gz
curl -fLO https://github.com/scottc2810-sys/free-direct-dials/releases/download/$VER/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```
The last line must print `free-direct-dials-…tar.gz: OK`. Then:
```bash
tar xzf free-direct-dials-$VER-linux-$ARCH.tar.gz
cd free-direct-dials-$VER-linux-$ARCH
ls
```
You should see `dials  deploy  docs  README.md  VERSION  ARCH`.

> **No internet download?** Copy the bundle from Windows with PuTTY's `pscp` instead, in a Windows PowerShell window:
> ```powershell
> pscp -i C:\keys\dials-vm.ppk C:\Downloads\free-direct-dials-v1.0.0-linux-amd64.tar.gz <USER>@<VM_IP>:/tmp/
> ```
> Then on the VM: `cd /root && tar xzf /tmp/free-direct-dials-v1.0.0-linux-amd64.tar.gz && cd free-direct-dials-v1.0.0-linux-amd64`

## 5. Run the installer (first pass)

Replace `203.0.113.7` with **your** public IP. For several, separate them with spaces: `"203.0.113.7 198.51.100.0/24"`.
```bash
ADMIN_IPS="203.0.113.7" bash deploy/install.sh
```
It takes 2–4 minutes and does the following:
- Installs Caddy, the firewall (SSH plus HTTPS from Cloudflare only), fail2ban, swap and automatic security updates.
- Creates the service user and directories.
- Writes `/etc/dials/dials.env` with freshly generated secrets: `SECRET_KEY`, the secret `ADMIN_PATH`, `BACKUP_KEY` and the webhook token.
- Installs the service and the release.

It ends by listing what is still missing, normally the **Turnstile keys**, the **Brevo key** and the **Cloudflare Origin certificate**. The next three steps fill those in.

> SSH becomes **key-only**: password login is turned off because your key is installed. Keep `dials-vm.ppk` safe; it is now the only way in, apart from your cloud provider's web console.

## 6. Cloudflare

All of this is in dash.cloudflare.com → your domain.

1. **DNS → Records → Add record:** Type `A`, Name `origin-dials`, IPv4 `<VM_IP>`, Proxy status **Proxied** (orange cloud).
2. **SSL/TLS → Overview:** encryption mode **Full (strict)**. If the main site's host can't do strict, see RUNBOOK §1.1 step 3 for a rule that applies it to this path only.
3. **SSL/TLS → Origin Server → Create Certificate:** key type RSA (2048), hostnames `markemindai.com` and `*.markemindai.com`, validity 15 years → **Create**. Keep the page open; you'll copy two blocks from it next.
4. Back in PuTTY, paste the **Origin Certificate** block (`-----BEGIN CERTIFICATE-----` … `-----END CERTIFICATE-----`):
   ```bash
   nano /etc/caddy/origin.pem
   ```
   Right-click to paste, then `Ctrl+O`, `Enter`, `Ctrl+X` to save and exit. Then the **Private Key** block (`-----BEGIN PRIVATE KEY-----` …):
   ```bash
   nano /etc/caddy/origin-key.pem
   ```
5. **Rules → Origin Rules → Create rule** "Free Direct Dials":
   - When *Custom filter expression*: `URI Path` *starts with* `/free-direct-dials` **OR** `URI Path` *starts with* `/free-direct-dails`.
   - Then **DNS record override** → `origin-dials.markemindai.com` → **Deploy**.
   - Optional: add `URI Path` *equals* `/ads.txt` if this server should serve ads.txt (MONETIZATION.md §4).
6. **Turnstile → Add widget:**
   - Name `Free Direct Dials`, hostname `markemindai.com`, mode **Managed** → Create.
   - Copy the **Site Key** and **Secret Key** for step 7.
7. **Security → WAF → Custom rules → Create rule** "Superadmin allow-list":
   - Expression:
     ```
     (http.request.uri.path contains "/free-direct-dials/<ADMIN_PATH>") and not (ip.src in {203.0.113.7})
     ```
     Use your IPs. Read `<ADMIN_PATH>` on the VM with `grep ADMIN_PATH /etc/dials/dials.env`.
   - Action **Block** → Deploy.
8. **Caching → Cache Rules:** create the three rules in RUNBOOK §1.1 step 6: assets cached a year; personal and admin paths bypassed; everything else follows the origin's headers.
9. **Security → WAF → Rate limiting rule:**
   - When `URI Path` *contains* `/free-direct-dials/api/auth/`, allow 20 requests per 10 seconds.
   - Over that, **Block** for 1 minute.

## 7. Email (Brevo)

1. brevo.com → sign up → **Senders, Domains & Dedicated IPs → Domains → Add a domain** `markemindai.com`.
2. Add the **SPF, DKIM and DMARC** records Brevo shows to Cloudflare DNS (type TXT/CNAME, **DNS only**, grey cloud). Wait until Brevo shows them verified.
3. **SMTP & API → API Keys → Generate a new API key.** Copy it.

## 8. Fill in the remaining settings, then finish

```bash
nano /etc/dials/dials.env
```
Fill these lines (arrow keys to move):

| Line | Value |
|---|---|
| `TURNSTILE_SITE_KEY=` | Site Key from step 6.6 |
| `TURNSTILE_SECRET=` | Secret Key from step 6.6 |
| `BREVO_API_KEY=` | Key from step 7.3 |
| `MAIL_PROVIDERS=` | `brevo:300` (add `,resend:100` only if you also set `RESEND_API_KEY`) |
| `ADMIN_EMAIL=` | where alerts go (disk full, database down, import failed) |
| `MAIL_FROM=` | e.g. `no-reply@markemindai.com` (on the domain verified in Brevo) |

Leave `SECRET_KEY`, `ADMIN_PATH`, `BACKUP_KEY` and `ADMIN_ALLOW_CIDRS` as the installer wrote them. Save with `Ctrl+O`, `Enter`, `Ctrl+X`.

Run the installer again, still from the bundle folder, with the same IP:
```bash
cd /root/free-direct-dials-v1.0.0-linux-$(dpkg --print-architecture)
ADMIN_IPS="203.0.113.7" bash deploy/install.sh
```
This time it checks the configuration, starts the app and Caddy, and prints:
```
Free Direct Dials v1.0.0 is running.
  Site:         https://markemindai.com/free-direct-dials/
  Superadmin:   https://markemindai.com/free-direct-dials/<ADMIN_PATH>/
```
**Copy the `BACKUP_KEY` it prints into your password manager now.** Without it, backups can't be restored.

## 9. Create your superadmin login

```bash
dials admin create yourname
```
1. Type a password of at least 12 characters and press Enter. The characters don't show; that's normal.
2. A QR code appears. Scan it with Google Authenticator, 1Password, Authy or similar.
   - If the QR looks garbled, set PuTTY to UTF-8 (step 3.5) and run the command again.
   - Or type the key printed under the QR into the app manually.
3. Open the **Superadmin** URL from step 8 in your browser, **from the IP you allowed**. Sign in with the username, the password and the 6-digit code.

## 10. First settings (in the browser)

Work through the dashboard warnings, roughly in this order:

1. **Site & legal:** legal name, postal address and privacy contact email. They appear in every email and are required by anti-spam law.
2. **Ads & Smart Link:** paste your Adsterra banner codes. Optionally add **VAST video** tags (HilltopAds etc.) or a Google rewarded ad unit, then **ads.txt**. See [MONETIZATION.md](MONETIZATION.md).
3. **WhatsApp:** already set to +91 98343 46179. Change the message if you like.
4. **Donations:** add your **UPI ID**, PayPal link and wallets.
5. **Partner offers:** paste your Apollo / Lusha affiliate links and tick *Live on the site*.
6. **Upload data:** CSV or Excel, map the columns, check, import. Optional: **Databases** to add Supabase projects for more room.
7. Google Search Console: add `https://markemindai.com/free-direct-dials/sitemap.xml`.

Check from your PC that it all works:
- Open the site, filter, sign in with a code, take a sponsor break and unlock 10 contacts.
- Open the superadmin URL from a phone on mobile data. It must show **Not found**, because that's a different IP.

---

## Everyday commands (PuTTY, after `sudo -i`)

| What | Command |
|---|---|
| Is it running? | `systemctl status dials --no-pager` and `systemctl status caddy --no-pager` |
| Live logs | `journalctl -u dials -f` (`Ctrl+C` to stop) |
| Health | `curl -s http://127.0.0.1:8081/free-direct-dials/readyz` |
| Config check | `dials check` |
| Databases (VPS + Supabase) | `dials stores` |
| Back up now | `dials backup` (also runs nightly at 03:30 UTC; the newest 2 are kept in `/var/backups/dials`) |
| Import a big file from disk | `dials import /tmp/file.xlsx` (copy it up with `pscp` first, as in step 4) |
| Restart | `systemctl restart dials` |
| Disk | `df -h /` |
| Change the admin IPs | re-run step 5 with the new `ADMIN_IPS` from the bundle folder, and update the Cloudflare WAF rule (step 6.7) |

## Upgrading to a new release

```bash
sudo -i
cd /root
VER=v1.0.1                              # the new version
ARCH=$(dpkg --print-architecture)
curl -fLO https://github.com/scottc2810-sys/free-direct-dials/releases/download/$VER/free-direct-dials-$VER-linux-$ARCH.tar.gz
curl -fLO https://github.com/scottc2810-sys/free-direct-dials/releases/download/$VER/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar xzf free-direct-dials-$VER-linux-$ARCH.tar.gz && cd free-direct-dials-$VER-linux-$ARCH
bash deploy/upgrade.sh
```
`upgrade.sh` does the following:
1. Backs up the database.
2. Switches to the new release and restarts the app.
3. Waits for the health check. If the new release isn't healthy within 40 seconds, it **switches back automatically** and shows the log.
4. Keeps the last 5 releases in `/opt/dials/releases`.

## Copying backups to your PC

On the VM:
```bash
cp /var/backups/dials/*.enc /home/<USER>/ && chown <USER> /home/<USER>/*.enc
```
In Windows PowerShell:
```powershell
pscp -i C:\keys\dials-vm.ppk "<USER>@<VM_IP>:/home/<USER>/*.enc" C:\backups\
```
Then on the VM: `rm /home/<USER>/*.enc`. Backups are encrypted with `BACKUP_KEY`. Restore steps: RUNBOOK §4.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `sha256sum: … FAILED` | incomplete download | delete the `.tar.gz` and download it again |
| `this bundle is linux-amd64 but this VM is aarch64` | wrong bundle for the CPU | download the `linux-arm64` bundle |
| Installer: `the configuration has errors` | a value in dials.env is missing or wrong (the line above says which) | `nano /etc/dials/dials.env`, fix it, re-run step 8 |
| Cloudflare **521 / 522** | 443 blocked between Cloudflare and the VM | cloud firewall/security list must allow TCP 443; Oracle: see step 2; `ufw status` should list 443 for Cloudflare ranges |
| Cloudflare **525 / 526** | Origin certificate missing or wrong | redo step 6.3–6.4; `journalctl -u caddy -n 30` |
| Cloudflare **404** on the site | Origin Rule missing, so the request went to the main site | step 6.5 |
| Superadmin shows **Not found** from your PC | your IP changed, or it isn't in the list | check your IP; re-run step 5 with the new IP and update the WAF rule |
| Sign-in code never arrives | Brevo key, sender domain not verified, or daily cap | Superadmin → Email → *Send test email*; `journalctl -u dials -n 50` |
| `readyz` says disk level 3 | disk ≥ 95 % (the site turns read-only) | `df -h /`; delete old files in `/root`; grow the disk |
| Locked out of SSH | key lost | use the cloud provider's web console; add a new public key to `~/.ssh/authorized_keys` |

More detail on Cloudflare caching, alerts, restores and scaling: [RUNBOOK.md](RUNBOOK.md).
