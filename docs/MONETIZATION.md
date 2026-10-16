# Monetisation guide: which networks, how to sign up, and how to set them up

> Written 2026-10-16. Payout thresholds and commission rates change; check each network's own dashboard before relying on a figure. Sources are at the end.

## The short version

| Order | What | Why it fits this site | Where it goes in Superadmin |
|---|---|---|---|
| **1. Now** | **Adsterra** (you already have the Smart Link) | Easiest approval for a new site; display, native and Smart Link formats | Ads & Smart Link → slots + Smart Link |
| **1. Now** | **Partner offers (affiliates)**: Apollo.io and Lusha via PartnerStack | Your visitors want *more B2B data*, so a data-tool referral is the most relevant offer. Recurring commission on paying referrals | Partner offers |
| **1. Now** | **VAST video break**: HilltopAds (no traffic minimum) and/or Adsterra video tags | Paid video today without waiting for Google; same "watch to unlock" flow | Ads & Smart Link → mode *VAST video break* + tag URLs |
| **1. Now** | **WhatsApp bulk / custom data** (+91 98343 46179, already live) | One custom-list sale can be worth more than months of ad revenue | WhatsApp |
| **2. After 2–4 weeks of real traffic** | **Google AdSense** → **Google Ad Manager rewarded video** | Rewarded video is an explicit "watch to unlock", which fits the unlock model exactly | Ads & Smart Link → mode *Google rewarded* + ad unit path |
| 3. Backup | Monetag (one network for pops/vignettes, not two) | Low payout floor; useful if Adsterra fill drops in some country | Paste its code into a slot |
| 4. Later | Media.net, Ezoic, or a premium network | They want steady traffic first | Slots |

What the site does now (all switchable in Superadmin):

- **Unlock = one ad, 10 contacts, revealed the moment the ad ends.** Sponsor-break mode unlocks automatically when the countdown finishes. Google rewarded mode unlocks on Google's "reward granted" event. There's no extra click either way.
- **Every further preview page is one more impression.** Anyone can browse 5 pages of masked rows per filter. Page 1 is free; pages 2–5 each open after a 5-second sponsor break, once per page per visit.
- **Fallbacks never cost a visitor.** If Google has no rewarded video, the same unlock becomes a sponsor break. If an ad slot has no code, a labelled Smart Link banner fills it.
- **High-intent moments get partner offers and WhatsApp.** These are "you've unlocked everything", "daily limit reached" and "no results". That is when people want more data.

## Why this mix

- **The audience is B2B** (sales, recruiters, founders). Advertisers pay more for them than for general consumer traffic, and data-tool affiliates convert well because the visitor is already looking for contacts.
- **The value exchange is explicit.** People knowingly trade a short ad for 10 contacts. Rewarded formats, where the user opts in, are designed for exactly that, and they pay better than banners that people ignore.
- **Visitors come back.** Up to 50 unlocks a day per user, plus "new data" emails, bring the same people back to watch more ads.
- **Never pay for clicks.** Adsterra and Google both ban rewarding a click on an ad. The site rewards *viewing* (a timed break or a completed video), never clicking. Keep it that way.

## 0. VAST video break (paid video now, no traffic minimum)

Any network that hands you a **VAST tag URL** can fill the unlock break with a video. The page plays it with Google's free IMA player, and the 10 contacts unlock the moment the video ends.

1. **HilltopAds**: sign up as a publisher → add `markemindai.com` → choose **mainstream** (not adult) inventory → create a **Video VAST** zone → copy its VAST tag URL. No traffic minimum, weekly payouts.
2. **Adsterra**: in your publisher dashboard, check whether *Create code* offers a VAST / video format for your site. If it does, copy that URL too.
3. **Superadmin → Ads**:
   - Set *Unlock mode* = **VAST video break**.
   - Paste the tag URLs, one per line, best payer first.
   - Save.
4. How it behaves:
   - The visitor clicks *Watch a short video, unlock 10* → *Play the video*.
   - The first tag with a video plays (up to 5 are tried, 8 s each); the contacts unlock when it completes, or when a skippable ad is skipped.
   - No video from any tag means the normal banner sponsor break, so the visitor still gets the contacts.
   - Macros like `[CACHEBUSTER]`, `[TIMESTAMP]` and `{page_url}` are filled in automatically.
   - If the browser won't allow sound (no recent real click), the video plays muted.
   - When a network doesn't fill, the browser console shows why (`VAST tag 2 had no video: <code> <reason>`).
5. Once Google approves you, an **Ad Manager video ad unit's VAST tag** can go first in the same list. This is often simpler than the rewarded mode, and all your other tags stay as backups.
6. Verified with Google's public IMA sample tag: a broken first tag was skipped, the sample video played (about 10 s), and the contacts unlocked when it ended.

## 1. Adsterra (do this first, about 15 minutes)

1. Sign in at adsterra.com → **Websites → Add website**: `markemindai.com`, category *Business / Technology*. Approval for a new site is usually quick.
2. **Create codes** for:
   - **Banner 300×250** → paste into *Inside the sponsor break* (the most-viewed slot: every unlock and every preview page shows it) and into *Below results*.
   - **Banner 728×90**, plus **320×50** as the mobile code → *Top banner*.
   - **Native Banner** → *Below results*, if it fills better than the 300×250.
   - **Social Bar** (optional) → *Global*. It is intrusive; try it for a week and watch bounce rate.
3. Keep **Unlock mode = Sponsor break** (or Google rewarded later). Smart Link mode opens a new tab per unlock; use it only if Adsterra confirms in writing that it's allowed.
4. Payouts: third-party reviews report a low threshold via Paxum ($5), PayPal at $25 or more depending on account, wire at $1,000, on NET15. Confirm in your dashboard.
5. Copy Adsterra's **ads.txt line** (if they give one) into Superadmin → Ads → ads.txt.

## 2. Partner offers: Apollo.io and Lusha (about 20 minutes, then wait for approval)

Both run their affiliate programmes on **PartnerStack**:

1. Create a PartnerStack account (PayPal or Stripe for payouts).
2. Apply to **Apollo.io**. The PartnerStack listing says up to 20% for the first 12 months of each paying referral (15% monthly plans, 20% annual). Payouts come the month after, after a 30-day hold.
3. Apply to **Lusha** (lusha.com/affiliates). Approval is at their discretion; published rates vary between sources, so read your dashboard.
4. In each dashboard, copy your **tracking link** and paste it into **Superadmin → Partner offers**: brand, one-line pitch, button text, the link. Choose where it shows and tick *Live on the site*.
   - Best spots: **All-unlocked / daily-limit / no-results states** and **In the unlock box**.
   - Every card carries the "Partner offer" label and `rel="sponsored"`. That disclosure is required (FTC in the US, ASCI in India), so keep it.
5. Worth adding later: Hunter.io, Snov.io, Clay, ZoomInfo. Check each programme's terms.

## 3. Google AdSense → Ad Manager rewarded video (after you have real traffic)

Google's rewarded ads for web run through **Google Ad Manager** (GPT), and Ad Manager signup requires an AdSense account.

1. **AdSense:** apply at adsense.google.com with `markemindai.com`. The site needs real content and traffic. The SEO landing pages (country / industry lists), FAQ, privacy, terms and removal pages help. Approval can take days to weeks.
2. **Ad Manager:** go to admanager.google.com → *Create account*, using the same Google account as AdSense. The link to AdSense is permanent.
3. **Inventory → Ad units → New ad unit**:
   - Name `fdd_rewarded`, size **Out-of-page**.
   - Then *Inventory → Network settings*: make sure **"Block non-instream video ads" is off**. Rewarded can't serve otherwise.
4. **Delivery:** AdSense / Ad Exchange demand fills it once it's linked. Optionally add your own line items for direct sponsors.
5. In **Superadmin → Ads**:
   - Set *Unlock mode* = **Google rewarded video**.
   - Set *Ad Manager rewarded ad unit* = `/<your network code>/fdd_rewarded`.
   - Save, then test on your phone.

   Google's public test unit `/22639388115/rewarded_web_example` was used to verify the flow here: the video played, the reward was granted, and the contacts appeared at once.
6. How it behaves:
   - The visitor clicks **"Watch a short video, unlock 10"**, then **"Watch the video"** when it's ready. Google requires this explicit opt-in.
   - The contacts appear on Google's *reward granted* event.
   - If Google has nothing to show within about 8 seconds, the visitor gets the normal sponsor break instead.
   - Web rewarded ads have no server-side verification (it's app-only), so the server enforces a 5-second floor plus the daily caps.
7. **EU / UK visitors:** Google expects a Google-certified consent tool (CMP) for EEA/UK traffic. Until you add one (AdSense / Ad Manager → *Privacy & messaging* is free), the site asks Google for *limited ads* for visitors who haven't consented, and earnings there will be lower.
8. Add Google's **ads.txt line** (AdSense → Sites → ads.txt) in Superadmin → Ads → ads.txt.

## 4. ads.txt (needed by Google, recommended by everyone)

Ad networks only read **`https://markemindai.com/ads.txt`** at the domain root. A file under `/free-direct-dials/` is ignored. Pick one option:

- **A (simplest):** put the same lines into the main site's own `ads.txt`.
- **B:** let this server serve it. Add a Cloudflare **Origin Rule** for `URI Path equals "/ads.txt"` → DNS override `origin-dials.markemindai.com` (RUNBOOK §1.5). The Caddyfile already passes `/ads.txt` to the app, which serves whatever you paste in Superadmin → Ads → ads.txt. The main site must not also have its own file.

## 5. Monetag (optional backup)

Use **one** pop/vignette network at a time, or the experience gets unbearable. Monetag offers display, native, vignette and pop formats. Third-party listings report a $5 minimum withdrawal via PayPal, Skrill and WebMoney. Add its banner code to a slot only if Adsterra under-fills a region you care about.

## 6. The logic for best performance and maximum earnings

**Already built in, and the defaults are sensible:**

| Lever | Setting (Superadmin) | Effect |
|---|---|---|
| Ad per 10 contacts | Limits → *Contacts per sponsor break* = 10 | Matches the promise on the page; more ads per user than 25 or 50 per ad |
| Daily cap | Limits → *Breaks per user per day* = 50 | Up to 500 contacts per user per day while stopping scrapers |
| Rewarded first | Ads → *Google rewarded video* (once approved) | Highest-paying format, with automatic fallback |
| Preview gate | Ads → *Preview pages* on, 5 s | 4 extra impressions per filter from anonymous visitors |
| Partner offers | Partner offers → all-unlocked / limit / no-results states | Earns when ads can't (the visitor has hit a limit) |
| WhatsApp | WhatsApp → on | Custom-data sales; the message already includes the visitor's filters and match count |
| New-data emails | Imports → *Notify subscribers* after every upload | Brings users back, which means more ad views per user |
| Speed | Local-first database, mirror, CDN cache | Fast pages keep visitors browsing, and every page view is an impression |

**Do next** (no code needed):

1. **Upload fresh data often.** Every upload is a reason to email subscribers, and returning users are where most of the ad views come from.
2. **Grow the SEO pages.** Country and industry lists are rebuilt daily from your data. Submit the sitemap in Google Search Console and Bing.
3. **Test one change a week** and compare *sponsor breaks per day* on the dashboard with each network's revenue:
   - preview break 5 s vs 8 s
   - Native vs 300×250 in the sponsor break
   - Social Bar on vs off
4. **Watch Adsterra's per-country stats.** If India or other Tier-3 traffic earns little, the rewarded video or partner offers may beat banners there.

**Don't:**

- Reward clicks on ads, hide the "Sponsored" or "Partner offer" labels, or put ads on the removal, unsubscribe, privacy or terms pages. The code already prevents these.
- Stack two pop networks, or show pops on every click.
- Put contact details or emails in ad-request URLs (Google's PII rule). Filters live in the URL hash and unlocked data is never in a URL, so this holds today. Keep it that way if you change the code.

## Sources

- Google Ad Manager, *Traffic rewarded ads for web*: https://support.google.com/admanager/answer/9116812
- Google Publisher Tag, *Display a rewarded ad*: https://developers.google.com/publisher-tag/samples/display-rewarded-ad
- Google Ad Manager, *Get your first ads up and running* (AdSense account required): https://support.google.com/admanager/answer/7084151
- Google Ad Manager, *Declare authorized sellers (ads.txt)*: https://support.google.com/admanager/answer/9422161
- IAB Tech Lab ads.txt specification: https://unruly.co/wp-content/uploads/2017/08/IABOpenRTBAds.txtSpecification_Version1_Final.pdf
- Google AdSense publisher policies: https://support.google.com/adsense/answer/10502938
- Apollo.io on PartnerStack: https://market.partnerstack.com/page/apollo · case study: https://partnerstack.com/customers/apollo
- Lusha affiliates: https://www.lusha.com/affiliates/ · terms: https://www.lusha.com/legal/affiliate-program-terms/ · PartnerStack: https://market.partnerstack.com/page/lusha
- Adsterra payouts (third-party reviews): https://bloggerspassion.com/make-money-with-cpm-ads/ · https://www.adpushup.com/blog/adsterra-review-factsheet-overview-payment-pros-and-cons/
- Monetag (third-party listing): https://us.fitgap.com/products/monetag
- Supabase pricing (free plan limits): https://supabase.com/pricing
