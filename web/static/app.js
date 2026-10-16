/* Free Direct Dials client. Vanilla JS, no build step. All untrusted text goes through textContent. */
(() => {
  'use strict';
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const BASE = document.body.dataset.base || '';
  const PAGE = document.body.dataset.page || '';
  const NO_ADS = ['privacy', 'terms', 'remove', 'unsubscribe'].includes(PAGE);
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const nf = new Intl.NumberFormat();
  const fmt = n => nf.format(n || 0);
  const mq = q => window.matchMedia(q).matches;

  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? '' : v);
    }
    for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
    return el;
  }
  const ICONS = {
    copy: 'M9 9h10v10H9zM5 15V5h10',
    x: 'M6 6l12 12M18 6L6 18',
    chev: 'M9 6l6 6-6 6',
    out: 'M14 4h6v6M20 4l-9 9M18 14v6H4V6h6',
    lock: 'M6 11h12v9H6zM8.5 11V8a3.5 3.5 0 0 1 7 0v3',
    wa: 'M20 12a8 8 0 0 1-11.9 7l-4.1 1 1.1-3.9A8 8 0 1 1 20 12z',
    dl: 'M12 4v11M7 10l5 5 5-5M5 20h14',
  };
  const icon = n => {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    s.setAttribute('viewBox', '0 0 24 24'); s.setAttribute('class', 'ico'); s.setAttribute('aria-hidden', 'true');
    const p = document.createElementNS('http://www.w3.org/2000/svg', 'path'); p.setAttribute('d', ICONS[n]); s.append(p);
    return s;
  };

  let toastTimer;
  function toast(msg) {
    const t = $('#toast'); if (!t) return;
    t.textContent = msg; t.classList.add('on');
    clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove('on'), 2600);
  }
  async function copy(text, what) {
    try { await navigator.clipboard.writeText(text); toast((what || 'Copied') + ' copied'); }
    catch { const ta = h('textarea', { class: 'sr' }); ta.value = text; document.body.append(ta); ta.select(); document.execCommand('copy'); ta.remove(); toast('Copied'); }
  }
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
  };

  class ApiError extends Error {
    constructor(code, message, status, extra) { super(message); this.code = code; this.status = status; this.extra = extra || {}; }
  }
  async function api(path, { method = 'GET', body, signal } = {}) {
    const opt = { method, headers: { 'X-FDD': '1', Accept: 'application/json' }, credentials: 'same-origin', signal };
    if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
    for (let tries = 0; ; tries++) {
      let res;
      try { res = await fetch(BASE + path, opt); }
      catch (e) {
        if (e.name === 'AbortError') throw e;
        if (tries < 2) { await sleep(700 * (tries + 1)); continue; }
        throw new ApiError('network', "Can't reach the server. Check your connection and try again.", 0);
      }
      if (res.status === 503 && method === 'GET' && tries < 3) {
        await sleep(Math.min(+res.headers.get('Retry-After') || 3, 8) * 1000); continue;
      }
      const data = (res.headers.get('content-type') || '').includes('json') ? await res.json().catch(() => null) : null;
      if (!res.ok) throw new ApiError((data && data.error) || 'http_' + res.status, (data && data.message) || 'Something went wrong. Please try again.', res.status, data && data.extra);
      return data;
    }
  }

  // ---------- config, consent, placements, slots ----------
  let CFG = { settings: { ads: { slots: {}, placements: {} }, donations: {}, limits: {}, site: {} } };
  const S = () => CFG.settings;
  const signedHint = () => /(?:^|;\s*)fdd_in=1/.test(document.cookie);

  function needsConsent() {
    const mode = S().site.consent_mode;
    if (mode === 'off') return false;
    if (mode === 'all') return true;
    let tz = '';
    try { tz = Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch { /* old browsers */ }
    return /^Europe\//.test(tz) || ['Atlantic/Reykjavik', 'Atlantic/Canary', 'Atlantic/Madeira', 'Atlantic/Azores', 'Asia/Nicosia'].includes(tz);
  }
  const consentOK = () => !needsConsent() || store.get('fdd_consent') === 'yes';

  function consentBanner() {
    if (NO_ADS || !needsConsent() || store.get('fdd_consent')) return;
    const box = h('div', { class: 'consent', role: 'dialog', 'aria-label': 'Cookie choices' },
      h('p', {}, 'Ads keep this directory free. Our ad partners may use cookies to measure and personalise them. Sign-in uses one essential cookie either way. ',
        h('a', { href: BASE + '/privacy' }, 'Privacy policy')),
      h('div', { class: 'row' },
        h('button', { class: 'btn', type: 'button', onclick: () => { store.set('fdd_consent', 'no'); box.remove(); } }, 'Reject non-essential'),
        h('button', { class: 'btn btn-primary', type: 'button', onclick: () => { store.set('fdd_consent', 'yes'); box.remove(); renderSlots(); } }, 'Accept')));
    document.body.append(box);
  }

  const placed = name => !NO_ADS && !!S().ads.smartlink && !!(S().ads.placements || {})[name];
  function spAnchor(cls, ...kids) {
    return h('a', { class: cls, href: S().ads.smartlink, target: '_blank', rel: 'sponsored noopener' }, ...kids);
  }
  const spBadge = () => h('span', { class: 'sponsored' }, S().ads.label || 'Sponsored');

  function renderPlacements() {
    for (const el of $$('[data-place]')) {
      const name = el.dataset.place;
      el.replaceChildren();
      if (!placed(name)) continue;
      if (name === 'header') el.append(spAnchor('sp-link', spBadge(), S().ads.cta));
      else el.append(spAnchor('sp-link', spBadge(), ' ', S().ads.cta, ' ', icon('out')));
    }
  }

  function injectHTML(target, html) {
    const t = document.createElement('template');
    t.innerHTML = html; // admin-authored snippet in "direct" mode only
    for (const n of [...t.content.childNodes]) {
      if (n.nodeName === 'SCRIPT') {
        const s = document.createElement('script');
        for (const a of n.attributes) s.setAttribute(a.name, a.value);
        s.text = n.textContent; target.append(s);
      } else target.append(n);
    }
  }

  function houseAd(height) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 44 44'); svg.setAttribute('aria-hidden', 'true');
    svg.innerHTML = '<rect width="44" height="44" rx="8" fill="currentColor" opacity=".9"/><path d="M14 30l8-16 8 16M17.5 24h9" stroke="var(--paper)" stroke-width="2.6" fill="none" stroke-linecap="round" stroke-linejoin="round"/>';
    const a = spAnchor('house', svg, h('div', {}, h('strong', {}, S().ads.cta), h('span', {}, 'Our sponsor keeps every contact free.')),
      h('span', { class: 'btn btn-small' }, 'Open'));
    a.style.minHeight = Math.min(height || 90, 120) + 'px';
    return a;
  }

  function renderSlots() {
    if (NO_ADS) return;
    const mobile = mq('(max-width: 727px)');
    for (const el of $$('.slot[data-slot]')) {
      const name = el.dataset.slot;
      const sl = (S().ads.slots || {})[name];
      el.replaceChildren(); el.hidden = false;
      if (sl && consentOK()) {
        const hgt = (mobile && sl.hm) || sl.h || 90;
        el.style.setProperty('--h', hgt + 'px'); el.dataset.h = hgt;
        if (sl.isolation === 'direct') injectHTML(el, mobile && sl.html_mobile ? sl.html_mobile : sl.html);
        else el.append(h('iframe', { src: BASE + '/ad-frame?slot=' + encodeURIComponent(name) + (mobile ? '&m=1' : ''), title: 'Advertisement',
          loading: name === 'top' ? 'eager' : 'lazy', sandbox: 'allow-scripts allow-popups allow-popups-to-escape-sandbox', referrerpolicy: 'no-referrer-when-downgrade' }));
        el.after(h('span', { class: 'slot-label' }, 'Advertisement'));
      } else if (placed('slot_fallback') && name !== 'sponsor') {
        el.append(houseAd(sl ? sl.h : (name === 'incontent' ? 120 : 90)));
      } else el.hidden = true;
    }
    const g = (S().ads.slots || {}).global;
    if (g && consentOK() && !document.body.dataset.globalAd) { document.body.dataset.globalAd = '1'; injectHTML(document.body, g.html); }
  }

  async function adBlocked() {
    const probe = S().ads.probe_url;
    if (probe) { try { await fetch(probe, { mode: 'no-cors', cache: 'no-store' }); return false; } catch { return true; } }
    const bait = h('div', { class: 'adsbox ad-banner ad-slot pub_300x250', style: 'position:absolute;left:-9999px;height:10px;width:10px' });
    document.body.append(bait); await sleep(120);
    const blocked = bait.offsetHeight === 0 || getComputedStyle(bait).display === 'none';
    bait.remove(); return blocked;
  }

  // ---------- WhatsApp ----------
  function waURL(summary, count) {
    const w = S().whatsapp; if (!w) return '';
    let msg = w.message || '';
    if (summary) msg += '\n\nI was looking at: ' + summary + (count != null ? ' (' + fmt(count) + ' matches)' : '');
    return w.url + '?text=' + encodeURIComponent(msg);
  }
  function renderWhatsApp(summary, count) {
    const w = S().whatsapp;
    const top = $('#wa-top');
    if (top) {
      top.replaceChildren();
      if (w) top.append(h('a', { class: 'btn btn-small', href: waURL(summary, count), target: '_blank', rel: 'noopener' }, icon('wa'), w.label || 'Bulk data'));
    }
    const bulk = $('#bulk');
    if (bulk) { bulk.hidden = !w; const a = $('#wa-bulk'); if (a && w) a.href = waURL(summary, count); }
  }

  // ---------- donations ----------
  function renderDonations() {
    const d = S().donations, sec = $('#donate'), ul = $('#ways');
    if (!sec || !ul) return;
    const ways = [];
    if (d.enabled && d.upi) {
      const acts = [h('button', { class: 'btn btn-small', type: 'button', onclick: () => copy(d.upi.vpa, 'UPI ID') }, icon('copy'), 'Copy UPI ID')];
      if (mq('(pointer: coarse)')) acts.unshift(h('a', { class: 'btn btn-small btn-primary', href: d.upi.url }, 'Pay with UPI app'));
      ways.push(way('UPI', [h('div', { class: 'addr' }, d.upi.vpa), d.upi.name ? h('div', { class: 'note' }, d.upi.name) : null], acts, d.upi.qr, 'Scan with any UPI app'));
    }
    if (d.enabled && d.paypal) ways.push(way('PayPal', [h('div', { class: 'addr' }, d.paypal.replace(/^https:\/\//, ''))],
      [h('a', { class: 'btn btn-small btn-primary', href: d.paypal, target: '_blank', rel: 'noopener' }, 'Donate with PayPal')]));
    if (d.enabled && d.link) ways.push(way(d.link.label || 'Support', [h('div', { class: 'addr' }, d.link.url.replace(/^https:\/\//, ''))],
      [h('a', { class: 'btn btn-small', href: d.link.url, target: '_blank', rel: 'noopener' }, 'Open')]));
    for (const w of (d.enabled && d.wallets) || []) {
      const nets = (w.networks || []).map(n => n.name);
      ways.push(way(w.label || 'Crypto', [
        h('div', { class: 'addr' }, w.address),
        h('div', { class: 'nets' }, nets.map(n => h('span', {}, n))),
        h('p', { class: 'note' }, 'Send ' + (w.tokens || 'tokens') + ' only on ' + nets.join(', ') + '. Funds sent on any other network are lost.'),
      ], [h('button', { class: 'btn btn-small', type: 'button', onclick: () => copy(w.address, 'Address') }, icon('copy'), 'Copy address')], w.qr, 'Wallet QR code'));
    }
    sec.hidden = ways.length === 0;
    $('#donate-h').textContent = d.heading || 'Keep this directory free';
    $('#donate-msg').textContent = d.message || '';
    ul.replaceChildren(...ways);
  }
  function way(title, body, acts, qr, qrLabel) {
    const li = h('li', { class: 'way' }, h('b', {}, title), h('div', {}, body), h('div', { class: 'acts' }, acts));
    if (qr) {
      const box = h('div', { class: 'qr', role: 'img', 'aria-label': qrLabel });
      box.innerHTML = qr; // server-rendered SVG from validated settings
      li.append(h('details', {}, h('summary', {}, 'Show QR code'), box));
    }
    return li;
  }

  // ---------- sign-in widgets ----------
  let tsLoading;
  function turnstile(container, onToken) {
    if (!CFG.turnstile) { onToken('dev'); return { reset() {} }; }
    tsLoading = tsLoading || new Promise((res, rej) => {
      const s = h('script', { src: 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit', async: true });
      s.onload = res; s.onerror = () => rej(new Error('turnstile')); document.head.append(s);
    });
    let id = null;
    tsLoading.then(() => {
      id = window.turnstile.render(container, { sitekey: CFG.turnstile, callback: onToken, 'expired-callback': () => onToken(''), 'error-callback': () => onToken('') });
    }).catch(() => container.replaceChildren(h('p', { class: 'err' }, "The bot check couldn't load. Disable blockers for challenges.cloudflare.com and reload.")));
    return { reset() { if (id != null) { try { window.turnstile.reset(id); } catch { /* widget gone */ } } onToken(''); } };
  }

  // ======================================================================
  // Directory page
  // ======================================================================
  const DIMS = [
    ['country', 'Country'], ['state', 'State / region'], ['city', 'City'], ['industry', 'Industry'], ['sector', 'Sector'],
    ['seniority', 'Seniority'], ['division', 'Department'], ['decision', 'Decision power'], ['csize', 'Company size'],
    ['revenue', 'Revenue'], ['ctype', 'Company type'], ['econf', 'Email confidence'],
  ];
  const LABEL = Object.fromEntries(DIMS);
  const st = {
    sel: Object.fromEntries(DIMS.map(([k]) => [k, new Set()])), labels: {}, kw: '', tech: '', email: false, phone: false,
    open: new Set(['country', 'industry']), me: null, newest: false, seq: 0, fseq: {}, last: null, reveal: new Set(),
    dock: 'idle', token: null, pendingUnlock: false, total: null,
  };
  const bc = 'BroadcastChannel' in window ? new BroadcastChannel('fdd') : null;

  function qs(exclude) {
    const p = new URLSearchParams();
    for (const [k] of DIMS) if (k !== exclude) [...st.sel[k]].sort().forEach(v => p.append(k, v));
    const kw = st.kw.trim().toLowerCase().replace(/\s+/g, ' ');
    if (kw) p.set('kw', kw);
    const tech = st.tech.split(',').map(s => s.trim().toLowerCase()).filter(Boolean).sort().join(',');
    if (tech) p.set('tech', tech);
    if (st.email) p.set('email', '1');
    if (st.phone) p.set('phone', '1');
    p.sort();
    return p.toString();
  }
  function readHash() {
    const p = new URLSearchParams(location.hash.slice(1));
    for (const [k] of DIMS) st.sel[k] = new Set(p.getAll(k).slice(0, 50));
    st.kw = p.get('kw') || ''; st.tech = p.get('tech') || '';
    st.email = p.get('email') === '1'; st.phone = p.get('phone') === '1';
    $('#kw').value = st.kw; $('#tech').value = st.tech; $('#has-email').checked = st.email; $('#has-phone').checked = st.phone;
  }
  function writeHash() {
    const q = qs();
    history.replaceState(null, '', q ? '#' + q : location.pathname + location.search);
  }
  const activeCount = () => DIMS.reduce((n, [k]) => n + st.sel[k].size, 0) + (st.kw.trim() ? 1 : 0) + (st.tech.trim() ? 1 : 0) + st.email + st.phone;
  function summary() {
    const parts = [];
    for (const [k, l] of DIMS) if (st.sel[k].size) parts.push(l + ': ' + [...st.sel[k]].map(v => st.labels[k + '|' + v] || v).join(', '));
    if (st.kw.trim()) parts.push('Keyword: ' + st.kw.trim());
    if (st.tech.trim()) parts.push('Technology: ' + st.tech.trim());
    if (st.email) parts.push('Has email'); if (st.phone) parts.push('Has phone');
    return parts.join('; ');
  }

  let refreshTimer;
  function changed(delay = 150) {
    writeHash(); renderChips();
    clearTimeout(refreshTimer); refreshTimer = setTimeout(refresh, delay);
  }

  // ----- facets -----
  function buildFacets() {
    const box = $('#facets');
    box.replaceChildren(...DIMS.map(([k, l]) => {
      const sec = h('div', { class: 'fsec', 'data-dim': k });
      const btn = h('button', { type: 'button', 'aria-expanded': 'false', 'aria-controls': 'fb-' + k }, h('span', {}, l), h('span', { class: 'cnt', hidden: true }), h('span', { class: 'chev' }, icon('chev')));
      const search = h('input', { class: 'input', type: 'search', placeholder: 'Search ' + l.toLowerCase(), 'aria-label': 'Search ' + l, maxlength: '60' });
      const opts = h('div', { class: 'opts', role: 'group', 'aria-label': l });
      const body = h('div', { class: 'fbody', id: 'fb-' + k }, search, opts);
      btn.addEventListener('click', () => toggleFacet(k));
      let t; search.addEventListener('input', () => { clearTimeout(t); t = setTimeout(() => loadFacet(k), 250); });
      opts.addEventListener('change', e => {
        const cb = e.target; if (!cb.matches('input[type=checkbox]')) return;
        cb.checked ? st.sel[k].add(cb.value) : st.sel[k].delete(cb.value);
        updateBadges(); changed();
      });
      sec.append(btn, body);
      return sec;
    }));
    for (const k of st.open) setOpen(k, true);
    updateBadges();
  }
  function setOpen(k, open) {
    const sec = $(`.fsec[data-dim="${k}"]`); if (!sec) return;
    sec.toggleAttribute('data-open', open); $('button', sec).setAttribute('aria-expanded', String(open));
    open ? st.open.add(k) : st.open.delete(k);
  }
  function toggleFacet(k) { const open = !st.open.has(k); setOpen(k, open); if (open) loadFacet(k); }
  function updateBadges() {
    for (const [k] of DIMS) {
      const c = $(`.fsec[data-dim="${k}"] .cnt`); if (!c) continue;
      c.hidden = !st.sel[k].size; c.textContent = st.sel[k].size;
    }
    const n = activeCount(); $('#filters-n').textContent = n ? '(' + n + ')' : ''; $('#clear-all').hidden = !n;
  }
  async function loadFacet(k) {
    const sec = $(`.fsec[data-dim="${k}"]`); if (!sec) return;
    const opts = $('.opts', sec), q = $('input[type=search]', sec).value.trim();
    const my = (st.fseq[k] = (st.fseq[k] || 0) + 1);
    if (!opts.children.length) opts.replaceChildren(h('div', { class: 'opt-state' }, 'Loading…'));
    try {
      const r = await api('/api/facets?dim=' + k + (q ? '&q=' + encodeURIComponent(q) : '') + (qs() ? '&' + qs() : ''));
      if (my !== st.fseq[k]) return;
      for (const v of r.values) st.labels[k + '|' + v.v] = v.l;
      const rows = r.values.map(v => h('label', { class: 'opt' + (v.n ? '' : ' zero') },
        h('input', { type: 'checkbox', value: v.v, checked: st.sel[k].has(v.v) }), h('span', { class: 'l', title: v.l }, v.l), h('span', { class: 'n' }, fmt(v.n))));
      if (!rows.length) rows.push(h('div', { class: 'opt-state' }, q ? 'No ' + LABEL[k].toLowerCase() + ' matches "' + q + '".' : 'No values with the current filters.'));
      if (r.truncated) rows.push(h('div', { class: 'opt-state' }, 'Showing the top ' + r.values.length + '. Search to find the rest.'));
      opts.replaceChildren(...rows);
      if (st.sel[k].size) renderChips(); // chips restored from the URL now get their proper labels
    } catch (e) {
      if (my === st.fseq[k]) opts.replaceChildren(h('div', { class: 'opt-state' }, e.message, ' ', h('button', { class: 'btn btn-small', type: 'button', onclick: () => loadFacet(k) }, 'Retry')));
    }
  }
  const refreshFacets = () => { for (const k of st.open) loadFacet(k); };

  function renderChips() {
    const box = $('#chips'); const chips = [];
    const chip = (label, value, onRemove) => h('span', { class: 'chip' }, h('b', {}, label), h('span', { title: value }, value),
      h('button', { type: 'button', 'aria-label': 'Remove ' + label + ' ' + value, onclick: onRemove }, icon('x')));
    const pretty = v => v === '__blank__' ? '(Blank)' : v.replace(/(^|[\s-])(\p{L})/gu, (m, a, b) => a + b.toUpperCase()); // until the facet's real label arrives
    for (const [k, l] of DIMS) for (const v of st.sel[k]) chips.push(chip(l, st.labels[k + '|' + v] || pretty(v), () => {
      st.sel[k].delete(v); updateBadges(); syncChecks(k); changed(0);
    }));
    if (st.kw.trim()) chips.push(chip('Keyword', st.kw.trim(), () => { st.kw = ''; $('#kw').value = ''; changed(0); }));
    if (st.tech.trim()) chips.push(chip('Tech', st.tech.trim(), () => { st.tech = ''; $('#tech').value = ''; changed(0); }));
    if (st.email) chips.push(chip('Has', 'email', () => { st.email = false; $('#has-email').checked = false; changed(0); }));
    if (st.phone) chips.push(chip('Has', 'phone', () => { st.phone = false; $('#has-phone').checked = false; changed(0); }));
    box.replaceChildren(...chips); updateBadges();
  }
  const syncChecks = k => $$(`.fsec[data-dim="${k}"] input[type=checkbox]`).forEach(cb => { cb.checked = st.sel[k].has(cb.value); });

  // ----- rows -----
  function maskedNode(v) {
    if (!v) return h('span', { class: 'muted' }, 'none');
    const span = h('span', { 'aria-label': 'hidden until unlocked' });
    v.split(/(•+)/).forEach(part => { if (!part) return; span.append(part.startsWith('•') ? h('span', { class: 'redact' + (part.length < 3 ? ' s' : '') }) : part); });
    return span;
  }
  function rowEl(r, i) {
    const tr = h('tr', { class: r.new ? 'is-new' : '' });
    const loc = [r.city, r.state, r.country].filter(Boolean).join(', ');
    tr.append(
      h('td', { class: 'ln' }, String(i + 1)),
      h('td', { class: 'who', 'data-k': 'Name' }, r.name || '—', r.new ? h('span', { class: 'tag-new' }, 'NEW') : null, h('small', {}, [r.title, r.seniority].filter(Boolean).join(' · '))),
      h('td', { class: 'co', 'data-k': 'Company' }, r.company || '—', h('small', {}, [r.industry, r.csize].filter(Boolean).join(' · '))),
      h('td', { 'data-k': 'Location' }, loc || '—'));
    if (r.masked) {
      tr.append(h('td', { class: 'data', 'data-k': 'Email' }, maskedNode(r.email)), h('td', { class: 'data', 'data-k': 'Phone' }, maskedNode(r.phone)),
        h('td', { 'data-k': 'LinkedIn' }, r.linkedin ? h('span', { class: 'redact' }) : h('span', { class: 'muted' }, '—')));
    } else {
      const cpy = (v, what) => h('button', { class: 'cp', type: 'button', 'aria-label': 'Copy ' + what, title: 'Copy ' + what, onclick: () => copy(v, what) }, icon('copy'));
      tr.append(
        h('td', { class: 'data', 'data-k': 'Email' }, r.email ? [h('a', { href: 'mailto:' + r.email }, r.email), cpy(r.email, 'Email')] : h('span', { class: 'muted' }, 'none')),
        h('td', { class: 'data', 'data-k': 'Phone' }, r.phone ? [h('a', { href: 'tel:' + r.phone.replace(/[^\d+]/g, '') }, r.phone), cpy(r.phone, 'Phone')] : h('span', { class: 'muted' }, 'none')),
        h('td', { 'data-k': 'LinkedIn' }, r.linkedin ? h('a', { class: 'li', href: /^https?:/.test(r.linkedin) ? r.linkedin : 'https://' + r.linkedin, target: '_blank', rel: 'noopener nofollow' }, 'Profile') : h('span', { class: 'muted' }, '—')));
    }
    return tr;
  }
  function skeleton() {
    const rows = [];
    for (let i = 0; i < 6; i++) rows.push(h('tr', { class: 'skel' }, ...Array.from({ length: 7 }, (_, j) => h('td', {}, h('span', { style: 'width:' + [20, 70, 60, 50, 65, 55, 30][j] + '%' })))));
    $('#rows').replaceChildren(...rows);
  }
  function renderRows(rows, batched) {
    const out = []; let lastBatch = null;
    rows.forEach((r, i) => {
      if (batched && r.batch !== lastBatch) {
        lastBatch = r.batch;
        out.push(h('tr', { class: 'batch' }, h('td', { colspan: '7' }, 'Batch ' + r.batch + (r.new ? ' · newest' : ''))));
      }
      const tr = rowEl(r, i);
      if (st.reveal.has(r.id)) { tr.classList.add('reveal'); tr.style.setProperty('--i', String(out.length % 12)); }
      out.push(tr);
    });
    $('#rows').replaceChildren(...out);
    st.reveal.clear();
  }
  function setCount(total, unlocked) {
    const c = $('#count');
    st.total = total;
    if (unlocked == null) c.replaceChildren(h('span', { class: 'data' }, fmt(total)), total === 1 ? ' contact matches' : ' contacts match',
      h('small', {}, activeCount() ? 'with your filters' : 'in the whole directory'));
    else c.replaceChildren(h('span', { class: 'data' }, fmt(total)), ' match', h('small', {}, fmt(unlocked) + ' unlocked by you · ' + fmt(Math.max(0, total - unlocked)) + ' left'));
  }
  function showEmpty(kind) {
    const box = $('#empty'); box.hidden = false; box.className = 'empty'; box.replaceChildren();
    const w = S().whatsapp;
    if (kind === 'none') {
      const removable = $$('#chips .chip button').slice(0, 4);
      box.append(h('h3', {}, 'No contacts match all of these filters'),
        h('p', {}, 'Remove a filter to widen the search, or ask us to build this exact list.'),
        h('div', { class: 'chips' }, removable.map(b => h('button', { class: 'btn btn-small', type: 'button', onclick: () => b.click() }, icon('x'), b.getAttribute('aria-label')))),
        w ? h('a', { class: 'btn', href: waURL(summary(), 0), target: '_blank', rel: 'noopener' }, icon('wa'), 'Request this list on WhatsApp') : null);
    }
    if (kind === 'error') box.append(h('h3', {}, "Results didn't load"), h('p', {}, st.lastError || ''), h('button', { class: 'btn', type: 'button', onclick: refresh }, 'Try again'));
  }

  async function refresh() {
    const my = ++st.seq; const q = qs();
    $('#empty').hidden = true; $('#cap').hidden = true; skeleton();
    try {
      let total, rows, unlocked = null, view = null;
      if (st.me) {
        const [v, p] = await Promise.all([api('/api/me/view?' + q + (st.newest ? (q ? '&' : '') + 'newest=1' : '')), api('/api/preview?' + q)]);
        if (my !== st.seq) return;
        view = v; total = v.total; unlocked = v.unlocked;
        rows = v.rows.length ? v.rows : (v.unlocked === 0 ? p.rows : []);
        st.me.ads_today = v.ads_today; st.me.max_ads = v.max_ads; st.view = v;
        $('#cap').hidden = !v.capped;
        if (v.capped) $('#cap').textContent = 'Showing your latest ' + fmt(v.rows.length) + ' unlocked contacts here. Export CSV to get all ' + fmt(v.unlocked) + '.';
        $('#export').hidden = !v.unlocked; $('#export').href = BASE + '/api/me/export?' + q;
        $('#newest-wrap').hidden = !v.unlocked;
        renderRows(rows, v.rows.length > 0);
      } else {
        const p = await api('/api/preview?' + q);
        if (my !== st.seq) return;
        total = p.total; rows = p.rows; renderRows(rows, false);
      }
      setCount(total, unlocked);
      if (!total) { $('#rows').replaceChildren(); showEmpty('none'); }
      renderWhatsApp(summary(), total);
      renderDock(view);
    } catch (e) {
      if (my !== st.seq) return;
      if (e.status === 401 && st.me) { signedOut(); return refresh(); }
      st.lastError = e.message; $('#rows').replaceChildren(); showEmpty('error');
    }
    refreshFacets();
  }

  // ----- the unlock dock (state machine) -----
  const N = () => S().limits.contacts_per_ad || 10;
  function dockSet(state) { st.dock = state; renderDock(st.view); }
  function renderDock(view) {
    const d = $('#dock'); if (!d) return;
    const mobile = $('#mobile-cta');
    const left = view ? view.remaining : st.total;
    const site = S().site, w = S().whatsapp;
    const sponsorBtn = () => placed('empty_states') ? spAnchor('btn', spBadge(), S().ads.cta) : null;
    const waBtn = label => w ? h('a', { class: 'btn', href: waURL(summary(), st.total), target: '_blank', rel: 'noopener' }, icon('wa'), label) : null;
    mobile.hidden = true;
    if (['signin', 'code', 'break', 'claiming'].includes(st.dock)) return; // those states render themselves
    d.replaceChildren();
    if (site.read_only) {
      d.append(h('div', { class: 'notice warn' }, 'Unlocking is paused for maintenance. Search and counts still work. Please check back shortly.'));
      return;
    }
    if (!st.total) { d.hidden = true; return; }
    d.hidden = false;
    if (st.me && view && view.remaining === 0) {
      st.dock = 'done';
      d.append(h('h3', {}, "You've unlocked every contact for these filters"),
        h('p', {}, 'Widen your filters to find new people, or get a custom list built for you.'), h('div', { class: 'row' }, waBtn('Ask for a bigger list'), sponsorBtn()));
      return;
    }
    if (st.me && st.me.ads_today >= st.me.max_ads) {
      st.dock = 'limit';
      const now = new Date(), next = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1);
      const hrs = Math.max(1, Math.round((next - now) / 36e5));
      d.append(h('h3', {}, "That's today's " + fmt(st.me.max_ads * N()) + ' free contacts'),
        h('p', {}, 'Your limit resets in about ' + hrs + ' hour' + (hrs > 1 ? 's' : '') + '. Need more now? We do bulk exports.'), h('div', { class: 'row' }, waBtn('Get bulk data'), sponsorBtn()));
      return;
    }
    st.dock = 'idle';
    const btn = h('button', { class: 'btn btn-primary btn-big', type: 'button', 'data-unlock': '' }, icon('lock'), 'Unlock next ' + Math.min(N(), left || N()) + ' contacts');
    d.append(h('div', { class: 'grid2' },
      h('div', {}, h('h3', {}, st.me ? fmt(left) + ' contacts left for these filters' : 'See real emails and direct dials'),
        h('p', { class: 'muted' }, st.me ? 'Each short sponsor break unlocks ' + N() + ' you have never seen. ' + (st.me.max_ads - st.me.ads_today) + ' breaks left today.'
          : 'Free. Confirm your email once, then each short sponsor break unlocks ' + N() + ' contacts.')), btn));
    mobile.hidden = false;
  }

  async function startUnlock() {
    if (S().site.read_only) return;
    if (!st.me) { st.pendingUnlock = true; return signIn(); }
    dockSet('break');
    const d = $('#dock'); d.replaceChildren(h('p', { class: 'muted' }, 'Starting sponsor break…'));
    const mode = S().ads.mode;
    let tok;
    try { tok = await api('/api/ad/start', { method: 'POST', body: {} }); }
    catch (e) { return dockError(e); }
    st.token = tok.token;
    (mode === 'smartlink' ? smartlinkBreak : sponsorBreak)(tok);
  }

  function dockError(e) {
    if (e.status === 401) { signedOut(); st.pendingUnlock = true; return signIn(); }
    if (e.code === 'limit_reached') { st.me && (st.me.ads_today = st.me.max_ads); dockSet('idle'); return; }
    if (e.code === 'nothing_left') { dockSet('idle'); return refresh(); }
    st.dock = 'error';
    $('#dock').replaceChildren(h('div', { class: 'notice bad' }, e.message), h('div', { class: 'row' }, h('button', { class: 'btn', type: 'button', onclick: () => dockSet('idle') }, 'Back')));
  }

  function breakShell(title) {
    const d = $('#dock');
    const feed = h('div', { class: 'feed', role: 'progressbar', 'aria-label': 'Sponsor break progress', 'aria-valuemin': '0', 'aria-valuemax': '100' }, h('i'));
    const secs = h('span', { class: 'data' });
    const unlock = h('button', { class: 'btn btn-primary btn-big', type: 'button', disabled: true }, 'Unlock ' + N() + ' contacts');
    const note = h('p', { class: 'note' });
    d.replaceChildren(h('div', { class: 'row' }, h('h3', {}, title), h('span', { class: 'grow' }), secs), feed);
    return { d, feed, secs, unlock, note };
  }

  async function sponsorBreak(tok) {
    const { d, feed, secs, unlock, note } = breakShell('Sponsor break');
    const slot = h('div', { class: 'break-slot' });
    const sl = (S().ads.slots || {}).sponsor;
    if (sl && consentOK()) {
      if (sl.isolation === 'direct') injectHTML(slot, sl.html);
      else slot.append(h('iframe', { src: BASE + '/ad-frame?slot=sponsor' + (mq('(max-width: 727px)') ? '&m=1' : ''), title: 'Advertisement', sandbox: 'allow-scripts allow-popups allow-popups-to-escape-sandbox' }));
    } else if (placed('slot_fallback')) slot.append(houseAd(120));
    d.append(slot, h('div', { class: 'row' }, unlock, placed('sponsor_button') ? spAnchor('btn', spBadge(), S().ads.cta) : null, h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => dockSet('idle') }, 'Cancel')), note);
    const policy = S().ads.adblock;
    let blocked = false;
    if (policy !== 'allow') adBlocked().then(b => {
      blocked = b;
      if (b) note.textContent = policy === 'require' ? 'An ad blocker is on. Sponsors pay for this directory, so please allow ads on this site and reload to unlock.'
        : 'Looks like an ad blocker is on. Unlocking still works, but allowing ads here is what keeps it free.';
    });
    const need = tok.min_seconds * 1000; let shown = 0, last = performance.now();
    while (shown < need) {
      await sleep(250);
      if (st.dock !== 'break' || st.token !== tok.token) return;
      const now = performance.now();
      if (!document.hidden) shown += now - last; // the break only counts while the ad is visible
      last = now;
      $('i', feed).style.width = Math.min(100, (shown / need) * 100) + '%';
      feed.setAttribute('aria-valuenow', String(Math.round((shown / need) * 100)));
      secs.textContent = Math.max(0, Math.ceil((need - shown) / 1000)) + ' s';
    }
    secs.textContent = 'Ready';
    if (blocked && policy === 'require') { unlock.disabled = true; return; }
    unlock.disabled = false; unlock.focus({ preventScroll: true });
    unlock.addEventListener('click', () => claim(tok.token), { once: true });
  }

  function smartlinkBreak(tok) {
    const { d, feed, secs, unlock, note } = breakShell('Visit our sponsor');
    const need = tok.min_seconds * 1000; let away = 0, leftAt = null, opened = false;
    const open = h('a', { class: 'btn btn-primary btn-big', href: tok.smartlink, target: '_blank', rel: 'sponsored noopener' }, spBadge(), ' Open sponsor in a new tab');
    open.addEventListener('click', () => { opened = true; note.textContent = 'Keep the sponsor tab open for ' + tok.min_seconds + ' seconds, then come back here.'; });
    d.append(h('p', {}, 'Open the sponsor page, spend ' + tok.min_seconds + ' seconds there, then come back. Your ' + N() + ' contacts unlock automatically.'),
      h('div', { class: 'row' }, open, unlock, h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => dockSet('idle') }, 'Cancel')), note);
    const onVis = () => {
      if (document.hidden && opened) leftAt = performance.now();
      else if (leftAt != null) { away += performance.now() - leftAt; leftAt = null; tick(); }
    };
    document.addEventListener('visibilitychange', onVis);
    const tick = () => {
      if (st.dock !== 'break' || st.token !== tok.token) { document.removeEventListener('visibilitychange', onVis); return; }
      const total = away + (leftAt != null ? performance.now() - leftAt : 0);
      $('i', feed).style.width = Math.min(100, (total / need) * 100) + '%';
      secs.textContent = total >= need ? 'Ready' : Math.ceil((need - total) / 1000) + ' s';
      if (total >= need && !document.hidden) {
        document.removeEventListener('visibilitychange', onVis);
        unlock.disabled = false; claim(tok.token);
      }
    };
    const iv = setInterval(() => { if (st.dock !== 'break') { clearInterval(iv); return; } tick(); }, 500);
  }

  async function claim(token, retried) {
    st.dock = 'claiming';
    $('#dock').replaceChildren(h('p', { class: 'muted' }, 'Unlocking…'));
    try {
      const res = await api('/api/ad/claim', { method: 'POST', body: { token, filter: qs() } });
      res.rows.forEach(r => st.reveal.add(r.id));
      st.me.ads_today = res.ads_today;
      toast(res.rows.length + ' new contacts unlocked');
      bc && bc.postMessage({ t: 'sync' });
      st.dock = 'idle'; st.newest = false; $('#newest').checked = false;
      await refresh();
      $('#sheet').scrollIntoView({ behavior: mq('(prefers-reduced-motion: reduce)') ? 'auto' : 'smooth', block: 'start' });
    } catch (e) {
      if (e.code === 'too_early' && !retried) { await sleep((e.extra.wait_ms || 1000) + 300); return claim(token, true); }
      if (e.code === 'token_used' || e.code === 'token_invalid') { st.dock = 'idle'; return startUnlock(); }
      dockError(e);
    }
  }

  // ----- sign-in (inline, no modal) -----
  function signIn() {
    st.dock = 'signin';
    const d = $('#dock'); d.hidden = false; $('#mobile-cta').hidden = true;
    const email = h('input', { class: 'input', id: 'si-email', type: 'email', autocomplete: 'email', required: true, maxlength: '254', placeholder: 'you@company.com', 'aria-describedby': 'si-err' });
    const sub = h('input', { type: 'checkbox', checked: S().site.subscribe_default !== false });
    const err = h('p', { class: 'err', id: 'si-err', role: 'alert' });
    const send = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Email me a code');
    const tsBox = h('div');
    let tsTok = ''; const ts = turnstile(tsBox, t => { tsTok = t; });
    const form = h('form', { novalidate: true }, h('div', { class: 'grid2' }, h('div', { class: 'field' }, h('label', { for: 'si-email' }, 'Work email'), email), send),
      tsBox, h('label', { class: 'check' }, sub, h('span', {}, 'Email me when new contacts are added. Unsubscribe in one click.')), err,
      h('p', { class: 'note' }, 'By continuing you agree to the ', h('a', { href: BASE + '/terms' }, 'Terms'), ' and ', h('a', { href: BASE + '/privacy' }, 'Privacy Policy'), '. We never sell your email.'));
    form.style.display = 'grid'; form.style.gap = '12px';
    if (!S().limits.signups) form.prepend(h('div', { class: 'notice warn' }, 'New sign-ups are paused. Existing accounts can still sign in.'));
    d.replaceChildren(h('h3', {}, 'Confirm your email to unlock'), form, h('button', { class: 'btn btn-ghost btn-small', type: 'button', onclick: () => { st.pendingUnlock = false; dockSet('idle'); } }, 'Not now'));
    email.focus();
    form.addEventListener('submit', async e => {
      e.preventDefault(); err.textContent = ''; email.removeAttribute('aria-invalid');
      if (!email.value.trim() || !email.checkValidity()) { err.textContent = 'Enter the email address you actually read.'; email.setAttribute('aria-invalid', 'true'); return; }
      if (CFG.turnstile && !tsTok) { err.textContent = 'Please complete the quick check above first.'; return; }
      send.classList.add('loading'); send.disabled = true;
      try {
        const r = await api('/api/auth/start', { method: 'POST', body: { email: email.value.trim(), turnstile: tsTok } });
        codeStep(r.email, sub.checked);
      } catch (ex) {
        err.textContent = ex.message; email.setAttribute('aria-invalid', String(ex.code === 'invalid_email'));
        ts.reset();
      } finally { send.classList.remove('loading'); send.disabled = false; }
    });
  }
  function codeStep(email, subscribe) {
    st.dock = 'code';
    const d = $('#dock');
    const code = h('input', { class: 'input otp', id: 'si-code', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '7', required: true, 'aria-describedby': 'si-cerr' });
    const err = h('p', { class: 'err', id: 'si-cerr', role: 'alert' });
    const go = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Verify');
    const resend = h('button', { class: 'btn btn-ghost btn-small', type: 'button', disabled: true }, 'Resend code');
    let wait = 30; const t = setInterval(() => { wait--; resend.textContent = wait > 0 ? 'Resend in ' + wait + ' s' : 'Resend code'; resend.disabled = wait > 0; if (wait <= 0) clearInterval(t); }, 1000);
    resend.addEventListener('click', () => { clearInterval(t); signIn(); $('#si-email').value = email; });
    const form = h('form', { novalidate: true }, h('div', { class: 'grid2' }, h('div', { class: 'field' }, h('label', { for: 'si-code' }, 'Enter the 6-digit code'), code), go), err);
    form.style.display = 'grid'; form.style.gap = '10px';
    d.replaceChildren(h('h3', {}, 'Check your inbox'), h('p', { class: 'muted' }, 'We sent a code to ', h('b', {}, email), '. It expires in 10 minutes. Check spam if it isn\'t there in a minute.'),
      form, h('div', { class: 'row' }, resend, h('button', { class: 'btn btn-ghost btn-small', type: 'button', onclick: () => { clearInterval(t); signIn(); } }, 'Use a different email')));
    code.focus();
    const submit = async () => {
      err.textContent = ''; const c = code.value.replace(/\D/g, '');
      if (c.length !== 6) { err.textContent = 'The code has 6 digits.'; return; }
      go.classList.add('loading'); go.disabled = true;
      let dead = false;
      try {
        await api('/api/auth/verify', { method: 'POST', body: { email, code: c, subscribe } });
        clearInterval(t); await loadMe(); bc && bc.postMessage({ t: 'sync' });
        toast('Signed in as ' + email);
        st.dock = 'idle';
        if (st.pendingUnlock) { st.pendingUnlock = false; await refresh(); startUnlock(); } else refresh();
      } catch (ex) {
        err.textContent = ex.message; code.select();
        if (ex.code === 'otp_locked' || ex.code === 'otp_expired') { dead = true; clearInterval(t); resend.disabled = false; resend.textContent = 'Get a new code'; }
      } finally { go.classList.remove('loading'); go.disabled = dead; }
    };
    form.addEventListener('submit', e => { e.preventDefault(); submit(); });
    code.addEventListener('input', () => { if (code.value.replace(/\D/g, '').length === 6) submit(); });
  }

  async function loadMe() {
    try { st.me = await api('/api/me'); } catch { st.me = null; }
    renderMe();
  }
  function renderMe() {
    const el = $('#me'); if (!el) return;
    el.hidden = !st.me; el.replaceChildren();
    if (st.me) el.append(h('span', { class: 'muted', title: st.me.email }, st.me.email.replace(/^(.{1,14}).*(@.*)$/, '$1…$2')),
      h('button', { class: 'btn btn-ghost btn-small', type: 'button', onclick: logout }, 'Sign out'));
  }
  function signedOut() { st.me = null; renderMe(); }
  async function logout() {
    try { await api('/api/auth/logout', { method: 'POST', body: {} }); } catch { /* cookie cleared server-side anyway */ }
    signedOut(); bc && bc.postMessage({ t: 'sync' }); st.dock = 'idle'; refresh();
  }

  // ----- mobile filter sheet -----
  function openFilters() {
    const sheet = $('#filter-sheet'), rail = $('#rail');
    const foot = h('div', { class: 'sheet-foot' },
      h('button', { class: 'btn', type: 'button', onclick: () => { clearAll(); } }, 'Clear'),
      h('button', { class: 'btn btn-primary', type: 'button', onclick: () => sheet.close() }, 'Show results'));
    sheet.replaceChildren(rail, foot);
    sheet.addEventListener('close', () => { $('#tool').prepend(rail); }, { once: true });
    sheet.showModal();
  }
  function clearAll() {
    for (const [k] of DIMS) { st.sel[k].clear(); syncChecks(k); }
    st.kw = st.tech = ''; st.email = st.phone = false;
    $('#kw').value = $('#tech').value = ''; $('#has-email').checked = $('#has-phone').checked = false;
    changed(0);
  }

  function initDirectory() {
    buildFacets();
    readHash();
    renderChips();
    let t;
    const onText = () => { st.kw = $('#kw').value.slice(0, 200); st.tech = $('#tech').value.slice(0, 300); clearTimeout(t); t = setTimeout(() => changed(0), 300); };
    $('#kw').addEventListener('input', onText); $('#tech').addEventListener('input', onText);
    $('#has-email').addEventListener('change', e => { st.email = e.target.checked; changed(); });
    $('#has-phone').addEventListener('change', e => { st.phone = e.target.checked; changed(); });
    $('#clear-all').addEventListener('click', clearAll);
    $('#open-filters').addEventListener('click', openFilters);
    $('#newest').addEventListener('change', e => { st.newest = e.target.checked; refresh(); });
    document.addEventListener('click', e => { if (e.target.closest('[data-unlock]')) startUnlock(); });
    window.addEventListener('hashchange', () => { readHash(); for (const [k] of DIMS) syncChecks(k); renderChips(); refresh(); });
    bc && bc.addEventListener('message', async m => { if (m.data && m.data.t === 'sync') { await loadMe(); if (!['break', 'code', 'signin', 'claiming'].includes(st.dock)) refresh(); } });
    (signedHint() ? loadMe() : Promise.resolve()).then(refresh);
  }

  // ======================================================================
  // Removal & unsubscribe pages
  // ======================================================================
  function initRemove() {
    let tsTok = '', rqTok = '';
    turnstile($('#rm-ts'), t => { tsTok = t; });
    const rqTs = turnstile($('#rq-ts'), t => { rqTok = t; });
    let email = '';
    $('#rm-start').addEventListener('submit', async e => {
      e.preventDefault(); const msg = $('#rm-msg'); msg.className = ''; msg.textContent = '';
      const btn = $('button', e.target); btn.classList.add('loading');
      try {
        const r = await api('/api/remove/start', { method: 'POST', body: { email: $('#rm-addr').value.trim(), turnstile: tsTok } });
        email = r.email; $('#rm-verify').hidden = false; $('#rm-code').focus();
        msg.textContent = 'We sent a code to ' + email + '. Enter it to confirm.';
      } catch (ex) { msg.className = 'err'; msg.textContent = ex.message; } finally { btn.classList.remove('loading'); }
    });
    $('#rm-verify').addEventListener('submit', async e => {
      e.preventDefault(); const msg = $('#rm-msg'); msg.className = '';
      const btn = $('button', e.target); btn.classList.add('loading');
      try {
        const r = await api('/api/remove/verify', { method: 'POST', body: { email, code: $('#rm-code').value } });
        $('#rm-start').hidden = true; $('#rm-verify').hidden = true;
        msg.textContent = r.removed ? 'Done. Your record was deleted and blocked from future uploads.' : "We didn't find a record with that email, but we've blocked it from any future upload.";
      } catch (ex) { msg.className = 'err'; msg.textContent = ex.message; } finally { btn.classList.remove('loading'); }
    });
    $('#rm-request').addEventListener('submit', async e => {
      e.preventDefault(); const msg = $('#rq-msg'); msg.className = '';
      const btn = $('button[type=submit]', e.target); btn.classList.add('loading');
      try {
        await api('/api/remove/request', { method: 'POST', body: { kind: $('#rq-kind').value, value: $('#rq-value').value, contact: $('#rq-contact').value, note: $('#rq-note').value, turnstile: rqTok } });
        e.target.hidden = true; msg.textContent = 'Request received. We will remove the record within 30 days, usually much sooner.';
      } catch (ex) { msg.className = 'err'; msg.textContent = ex.message; rqTs.reset(); } finally { btn.classList.remove('loading'); }
    });
  }
  function initUnsub() {
    const f = $('#unsub-form'); if (!f) return;
    f.addEventListener('submit', async e => {
      e.preventDefault();
      try { await fetch(f.action, { method: 'POST', headers: { Accept: 'application/json' }, credentials: 'same-origin' }); f.hidden = true; $('#unsub-result').textContent = "Done. You won't get new-data emails any more."; }
      catch { f.submit(); }
    });
  }

  // ======================================================================
  async function boot() {
    window.addEventListener('offline', () => { if (!$('.offline')) document.body.append(h('div', { class: 'offline', role: 'status' }, "You're offline. Results will refresh when you reconnect.")); });
    window.addEventListener('online', () => { const o = $('.offline'); if (o) o.remove(); if (PAGE === 'index') refresh(); });
    try { CFG = await api('/api/config'); } catch { /* defaults keep the page usable */ }
    const ann = $('#announce');
    if (ann && S().site.announcement) { ann.textContent = S().site.announcement; ann.hidden = false; }
    renderPlacements(); renderSlots(); consentBanner(); renderWhatsApp('', null); renderDonations();
    if (PAGE === 'index') initDirectory();
    if (PAGE === 'remove') initRemove();
    if (PAGE === 'unsubscribe') initUnsub();
  }
  document.readyState === 'loading' ? document.addEventListener('DOMContentLoaded', boot) : boot();
})();
