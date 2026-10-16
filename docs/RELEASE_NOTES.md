## Deploy

Download the bundle for your VM's CPU (`dpkg --print-architecture` prints `amd64` or `arm64`) and **SHA256SUMS**, then follow **[docs/DEPLOY.md](https://github.com/scottc2810-sys/free-direct-dials/blob/main/docs/DEPLOY.md)**, the PuTTY step-by-step guide:

```bash
sudo -i
VER=<this tag>; ARCH=$(dpkg --print-architecture)
curl -fLO https://github.com/scottc2810-sys/free-direct-dials/releases/download/$VER/free-direct-dials-$VER-linux-$ARCH.tar.gz
curl -fLO https://github.com/scottc2810-sys/free-direct-dials/releases/download/$VER/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar xzf free-direct-dials-$VER-linux-$ARCH.tar.gz && cd free-direct-dials-$VER-linux-$ARCH
ADMIN_IPS="<your.public.ip>" bash deploy/install.sh     # first install
bash deploy/upgrade.sh                                  # or: upgrade an existing install
```

Each bundle contains:
- the `dials` server (a single static binary);
- `deploy/` with `install.sh`, `upgrade.sh`, `provision.sh`, the Caddyfile, the systemd unit and `dials.env.example`;
- `docs/` with DEPLOY, RUNBOOK and MONETIZATION.

## What's in it

- **Directory.** Free B2B contact directory with live counts and 12 filters, plus keyword and technology search. Job-title synonyms (CEO ↔ Chief Executive Officer, VP ↔ Vice Presidents…) and merged Seniority/Department values.
- **Unlocks.** Each ad unlocks 10 contacts, revealed the moment the ad ends. Unlock modes:
  - sponsor break;
  - **VAST video** (HilltopAds, Adsterra, any VAST tag);
  - **Google Ad Manager rewarded**;
  - Smart Link.
- **Paging.** Preview pages 2–5 each open after a short sponsor break; signed-in users page through all their unlocked contacts.
- **Contact databases.** The VPS SQLite database plus **Supabase/Postgres** databases, searched local-first and on demand, with the next database searched in the background. Each database is pinged every 6 hours.
- **Superadmin.** IP-allow-listed with TOTP. Includes:
  - CSV/Excel upload with column mapping and a dry run;
  - ads and ads.txt;
  - partner offers, donations (UPI, PayPal, crypto) and WhatsApp;
  - databases, removals, users and audit log.
- **Security and operations.** Encrypted backups, disk guard, alerts, CSRF, CSP, rate limits and a sandboxed systemd service.
