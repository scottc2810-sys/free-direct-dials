# Product brief: Free Direct Dials

## Job and audience
B2B sellers, SDRs, founders and recruiters, mostly in India and the US, at a desk during the working day. They need **real direct phone numbers and emails** for a defined slice of the market (country × industry × seniority × technology), and they want them now and free. They already know paid tools such as Apollo, Lusha and ZoomInfo, and they arrive sceptical that "free" is real.

## Surface modes
- **Directory (index):** Operate. The visitor completes a task: filter → see the count → unlock 10 contacts. The first viewport *is* the tool, with a one-sentence thesis above it.
- **SEO landing pages:** Persuade-lite. A real count, a masked sample and one action ("Filter these N contacts").
- **Superadmin:** Operate, dense. The operator imports data, maps columns, and manages money and compliance.
- **Legal, removal and unsubscribe:** Read. No ads.

## Proof the product must show (not claim)
- Live counts that change as filters change. Facet counts never lead to a dead end.
- Real rows with names, titles, companies and locations, with contact details as redaction bars until unlocked.
- After unlocking: real emails and phones, copy buttons, CSV export, and a NEW batch on top.

## Direction: "Blue-bar call sheet" (royal blue)
The visual world comes from the printed lead lists and call sheets salespeople worked from: continuous-form paper with alternating pale-blue bands, ruled lines, tabular figures, line numbers. Masked values are **redaction bars**. Newly unlocked rows get a **highlighter sweep**, the one authored motion, and stay highlighter-yellow with a NEW tag, the way a rep marks fresh leads. A single royal blue (`#2a4fd6`, lifted to `#7b9bff` at night) carries actions and state. WhatsApp keeps its own brand green because people recognise it. Light by default for daytime office use, with a "night shift" dark theme.

Refused category defaults: gradient SaaS hero, icon-card feature grid, hero-metric block, eyebrow labels, glassmorphism, modal-first flows (sign-in is inline in the unlock dock).

## Monetisation constraints (non-negotiable)
- Sponsored placements are always labelled, use `rel="sponsored"` and open a new tab. Nothing is disguised as navigation.
- The default unlock is a timed **sponsor break** that never requires a click (Adsterra bans incentivised clicks). Smart Link mode is opt-in.
- Ads never appear on privacy, terms, removal or unsubscribe pages.

## States that must exist
Loading (skeleton rows), zero results (remove-a-filter chips + WhatsApp "build this list"), all unlocked, daily limit (with reset time), maintenance / read-only, offline, rate-limited, busy (auto-retry), sign-in errors (disposable, typo domain, wrong or expired code, locked), ad blocker (allow / nudge / require).

## Open decisions for the founder
- Brand mark / logo (currently a drawn phone-handset glyph) and an optional self-hosted display face.
- Final legal copy (the templates in `web/templates/privacy.html` and `terms.html` must be reviewed by counsel).
