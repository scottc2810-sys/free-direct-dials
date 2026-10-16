/* Superadmin client. No inline styles (strict CSP); all text via textContent. */
(() => {
  'use strict';
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const PREFIX = document.body.dataset.prefix;
  const BASE = document.body.dataset.base || '';
  const nf = new Intl.NumberFormat();
  const fmt = n => nf.format(n || 0);
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const when = ts => ts ? new Date(ts * 1000).toLocaleString() : '—';
  const bytes = n => n > 1 << 30 ? (n / (1 << 30)).toFixed(2) + ' GB' : n > 1 << 20 ? (n / (1 << 20)).toFixed(1) + ' MB' : fmt(Math.round(n / 1024)) + ' KB';
  let CSRF = '', USER = '', FIELDS = null;

  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (k === 'value') el.value = v;
      else if (k === 'checked') el.checked = !!v;
      else el.setAttribute(k, v === true ? '' : v);
    }
    for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
    return el;
  }
  let tt;
  const toast = m => { const t = $('#toast'); t.textContent = m; t.classList.add('on'); clearTimeout(tt); tt = setTimeout(() => t.classList.remove('on'), 3000); };

  class ApiError extends Error { constructor(code, msg, status, extra) { super(msg); this.code = code; this.status = status; this.extra = extra; } }
  async function api(path, { method = 'GET', body, raw } = {}) {
    const opt = { method, headers: { 'X-CSRF': CSRF, Accept: 'application/json' }, credentials: 'same-origin' };
    if (raw !== undefined) { opt.body = raw; opt.headers['Content-Type'] = 'application/octet-stream'; }
    else if (body !== undefined) { opt.body = JSON.stringify(body); opt.headers['Content-Type'] = 'application/json'; }
    let res;
    try { res = await fetch(PREFIX + path, opt); } catch { throw new ApiError('network', 'Network error. Check your connection.', 0); }
    const data = (res.headers.get('content-type') || '').includes('json') ? await res.json().catch(() => null) : null;
    if (res.status === 401 && path !== '/api/login') { renderLogin(); throw new ApiError('unauthorized', 'Session expired.', 401); }
    if (!res.ok) throw new ApiError(data && data.error, (data && data.message) || 'Request failed (' + res.status + ')', res.status, data && data.extra);
    return data;
  }

  // ---------- login ----------
  function renderLogin(msg) {
    const err = h('p', { class: 'err', role: 'alert' }, msg || '');
    const u = h('input', { class: 'input', id: 'lu', autocomplete: 'username', required: true });
    const p = h('input', { class: 'input', id: 'lp', type: 'password', autocomplete: 'current-password', required: true });
    const c = h('input', { class: 'input otp', id: 'lc', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '6', required: true });
    const btn = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Sign in');
    const form = h('form', { class: 'login' }, h('h1', {}, 'Superadmin sign-in'),
      h('div', { class: 'field' }, h('label', { for: 'lu' }, 'Username'), u),
      h('div', { class: 'field' }, h('label', { for: 'lp' }, 'Password'), p),
      h('div', { class: 'field' }, h('label', { for: 'lc' }, 'Authenticator code'), c), btn, err,
      h('p', { class: 'note' }, 'No account yet? On the server run: dials admin create <username>'));
    form.addEventListener('submit', async e => {
      e.preventDefault(); err.textContent = ''; btn.classList.add('loading');
      try {
        const r = await api('/api/login', { method: 'POST', body: { username: u.value, password: p.value, code: c.value.trim() } });
        CSRF = r.csrf; USER = r.username; start();
      } catch (ex) { err.textContent = ex.message; c.value = ''; c.focus(); } finally { btn.classList.remove('loading'); }
    });
    $('#app').replaceChildren(form); u.focus();
  }

  // ---------- shell & routing ----------
  const VIEWS = [
    ['Overview', [['dashboard', 'Dashboard'], ['upload', 'Upload data'], ['imports', 'Imports & notify'], ['databases', 'Databases']]],
    ['Money', [['ads', 'Ads & Smart Link'], ['affiliates', 'Partner offers'], ['donations', 'Donations'], ['whatsapp', 'WhatsApp']]],
    ['Site', [['limits', 'Limits'], ['site', 'Site & legal'], ['email', 'Email']]],
    ['Data safety', [['removals', 'Removals'], ['blocklist', 'Blocklist'], ['users', 'Users'], ['audit', 'Audit log'], ['system', 'System']]],
  ];
  let pendingRemovals = 0;
  function shell() {
    const nav = h('nav', { class: 'adm-nav', 'aria-label': 'Admin' }, h('a', { class: 'brand', href: BASE + '/', target: '_blank', rel: 'noopener' }, 'Free Direct Dials ↗'));
    for (const [g, items] of VIEWS) {
      nav.append(h('div', { class: 'grp' }, g));
      for (const [id, label] of items) nav.append(h('a', { href: '#' + id, 'data-v': id }, label, id === 'removals' && pendingRemovals ? h('span', { class: 'badge' }, pendingRemovals) : null));
    }
    const main = h('main', { class: 'adm-main', id: 'view' });
    $('#app').replaceChildren(h('div', { class: 'adm-shell' }, nav, main));
  }
  function head(title, ...right) {
    return h('div', { class: 'adm-head' }, h('h1', {}, title), h('span', { class: 'grow' }), ...right,
      h('span', { class: 'adm-user' }, USER), h('button', { class: 'btn btn-small btn-ghost', type: 'button', onclick: logout }, 'Sign out'));
  }
  async function logout() { try { await api('/api/logout', { method: 'POST', body: {} }); } catch { /* ignore */ } CSRF = ''; renderLogin(); }
  let viewToken = 0;
  async function route() {
    const id = (location.hash || '#dashboard').slice(1);
    $$('.adm-nav a[data-v]').forEach(a => { if (a.dataset.v !== id) a.removeAttribute('aria-current'); else a.setAttribute('aria-current', 'page'); });
    const v = $('#view'); const my = ++viewToken;
    v.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
    try {
      const fn = VIEW_FN[id] || VIEW_FN.dashboard;
      const nodes = await fn(() => my === viewToken);
      if (my === viewToken) v.replaceChildren(...[].concat(nodes));
    } catch (e) { if (e.status !== 401 && my === viewToken) v.replaceChildren(head('Error'), h('p', { class: 'err' }, e.message)); }
  }

  async function start() {
    FIELDS = await api('/api/fields');
    try { const x = await api('/api/removals?status=pending'); pendingRemovals = (x.paging || {}).total || x.removals.length; } catch { pendingRemovals = 0; }
    shell(); route();
  }
  window.addEventListener('hashchange', () => CSRF && route());

  // pager renders "Previous · Page x of y · Next" for an admin list (paging = {page, pages, total})
  function pager(pg, go) {
    if (!pg || pg.pages <= 1) return null;
    return h('div', { class: 'actions mt' },
      h('button', { class: 'btn btn-small', type: 'button', disabled: pg.page === 0, onclick: () => go(pg.page - 1) }, 'Previous'),
      h('span', { class: 'note' }, 'Page ' + (pg.page + 1) + ' of ' + pg.pages + ' · ' + fmt(pg.total) + ' total'),
      h('button', { class: 'btn btn-small', type: 'button', disabled: pg.page >= pg.pages - 1, onclick: () => go(pg.page + 1) }, 'Next'));
  }

  // ---------- dashboard ----------
  const stat = (v, l) => h('div', { class: 'stat' }, h('b', {}, v), h('span', {}, l));
  async function vDashboard() {
    const s = await api('/api/stats');
    const hl = s.headline, u = s.users, ul = s.unlocks, ob = s.outbox;
    return [head('Dashboard', h('a', { class: 'btn btn-primary', href: '#upload' }, 'Upload data')),
      s.warnings.length ? h('ul', { class: 'warns' }, s.warnings.map(w => h('li', {}, w))) : null,
      h('div', { class: 'stats' },
        stat(fmt(hl.contacts), 'contacts'), stat(fmt(hl.with_email), 'with email'), stat(fmt(hl.with_phone), 'with phone'), stat(fmt(hl.countries), 'countries'),
        stat(fmt(u.total), 'users'), stat(fmt(u.subscribed), 'subscribed'), stat(fmt(u.today), 'sign-ups today'), stat(fmt(u.active_7d), 'active 7 days'),
        stat(fmt(ul.ads_today), 'sponsor breaks today'), stat(fmt(ul.users_today), 'unlocking users today'), stat(fmt(ul.breaks_total), 'sponsor breaks (all time)'),
        stat(fmt(ob.queued), 'emails queued'), stat(s.disk.pct.toFixed(0) + '%', 'disk used'), stat(bytes(s.db.bytes), 'database'),
        stat(bytes(s.db.wal_bytes), 'WAL'), stat(s.runtime.heap_mb + ' MB', 'heap')),
      h('div', { class: 'card' }, h('h2', {}, 'Quick actions'), h('div', { class: 'actions' },
        h('a', { class: 'btn', href: '#imports' }, 'Notify subscribers'),
        h('button', { class: 'btn', type: 'button', onclick: () => runJob('backup') }, 'Back up now'),
        h('button', { class: 'btn', type: 'button', onclick: purge }, 'Purge caches'),
        h('button', { class: 'btn', type: 'button', onclick: () => runJob('seo') }, 'Rebuild SEO pages'),
        h('a', { class: 'btn', href: '#site' }, s.read_only ? 'Turn off maintenance mode' : 'Maintenance mode'))),
      h('div', { class: 'card' }, h('h2', {}, 'Contact databases'), storesTable(s.stores || [], false), h('div', { class: 'actions mt' }, h('a', { class: 'btn', href: '#databases' }, 'Manage databases'))),
      h('div', { class: 'card' }, h('h2', {}, 'Email quota today'), usageTable(ob)),
    ];
  }
  function usageTable(ob) {
    return h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 'Provider'), h('th', {}, 'Sent'), h('th', {}, 'Daily cap'), h('th', {}, 'Campaign share'))),
      h('tbody', {}, (ob.usage || []).map(x => h('tr', {}, h('td', {}, x.provider), h('td', { class: 'num' }, fmt(x.sent)), h('td', { class: 'num' }, fmt(x.cap)), h('td', { class: 'num' }, fmt(x.bulk_cap)))))));
  }
  async function runJob(name) { try { const r = await api('/api/jobs/' + name + '/run', { method: 'POST', body: {} }); toast(r.started ? name + ' started' : name + ' is already running'); } catch (e) { toast(e.message); } }
  async function purge() { try { await api('/api/cache/purge', { method: 'POST', body: {} }); toast('Caches purged'); } catch (e) { toast(e.message); } }

  // ---------- upload wizard ----------
  const CHUNK_PARALLEL = 3;
  let upState = null;
  async function vUpload() {
    upState = { file: null, id: null, sheet: 0, preview: null, mapping: null, signature: '' };
    const box = h('div', { id: 'up' });
    renderUpStep1(box);
    return [head('Upload data'), h('div', { class: 'steps-adm', id: 'up-steps' }, ['1 Upload', '2 Map columns', '3 Check', '4 Import'].map((t, i) => h('span', { class: i === 0 ? 'on' : '' }, t))), box];
  }
  const stepOn = n => $$('#up-steps span').forEach((s, i) => s.classList.toggle('on', i === n));

  function renderUpStep1(box) {
    stepOn(0);
    const input = h('input', { type: 'file', accept: '.csv,.tsv,.txt,.gz,.xlsx,text/csv,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', class: 'sr', id: 'up-file' });
    const drop = h('label', { class: 'drop', for: 'up-file' }, h('strong', {}, 'Drop a CSV or Excel (.xlsx) file, or click to choose'),
      h('span', { class: 'note' }, 'CSV, TSV, semicolon CSV, .csv.gz or .xlsx, up to 5 GB. Large files upload in 8 MB chunks and resume if the connection drops.'), input);
    const prog = h('div', { class: 'card hide' }, h('h2', {}, 'Uploading'), h('div', { class: 'bar' }, h('i')), h('p', { class: 'note mt' }));
    ['dragenter', 'dragover'].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.add('over'); }));
    ['dragleave', 'drop'].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.remove('over'); }));
    drop.addEventListener('drop', e => { if (e.dataTransfer.files[0]) upload(e.dataTransfer.files[0], prog, box); });
    input.addEventListener('change', () => { if (input.files[0]) upload(input.files[0], prog, box); });
    box.replaceChildren(h('div', { class: 'card' }, drop), prog,
      h('div', { class: 'card' }, h('h2', {}, 'Tips'), h('ul', {},
        h('li', {}, 'Re-uploading the same file is safe: existing contacts are merged by email, then LinkedIn, then name + company. Nothing is duplicated.'),
        h('li', {}, 'Old .xls (Excel 97-2003) files: open in Excel and Save As .xlsx or CSV first.'),
        h('li', {}, 'Very large files can also be copied to the server and imported with: dials import file.csv'))));
  }

  async function upload(file, prog, box) {
    prog.classList.remove('hide');
    const bar = $('.bar i', prog), note = $('p', prog);
    const key = 'up:' + file.name + ':' + file.size + ':' + file.lastModified;
    try {
      let meta = null, id = null;
      try { id = sessionStorage.getItem(key); } catch { id = null; }
      if (id) { try { meta = (await api('/api/uploads/' + id)).upload; } catch { meta = null; } }
      if (!meta) { meta = await api('/api/uploads', { method: 'POST', body: { filename: file.name, size: file.size } }); try { sessionStorage.setItem(key, meta.id); } catch { /* ignore */ } }
      upState.id = meta.id; upState.file = file;
      const todo = meta.received.map((ok, i) => ok ? -1 : i).filter(i => i >= 0);
      let done = meta.chunks - todo.length;
      const t0 = Date.now();
      const show = () => { bar.style.width = (done / meta.chunks * 100) + '%'; note.textContent = fmt(done) + ' / ' + fmt(meta.chunks) + ' chunks · ' + bytes(Math.min(file.size, done * meta.chunk_size)) + ' of ' + bytes(file.size); };
      show();
      const worker = async () => {
        while (todo.length) {
          const i = todo.shift();
          const blob = file.slice(i * meta.chunk_size, Math.min(file.size, (i + 1) * meta.chunk_size));
          for (let attempt = 0; ; attempt++) {
            try { await api('/api/uploads/' + meta.id + '/chunks/' + i, { method: 'PUT', raw: blob }); break; }
            catch (e) { if (attempt >= 5 || e.status === 400 && e.code !== 'bad_chunk') throw e; await sleep(800 * 2 ** attempt); }
          }
          done++; show();
        }
      };
      await Promise.all(Array.from({ length: CHUNK_PARALLEL }, worker));
      note.textContent = 'Uploaded ' + bytes(file.size) + ' in ' + Math.round((Date.now() - t0) / 1000) + ' s. Reading the file…';
      await preview(box, 0);
    } catch (e) { note.textContent = ''; note.append(h('span', { class: 'err' }, e.message), ' Choose the file again to resume where it stopped.'); }
  }

  async function preview(box, sheet) {
    const r = await api('/api/uploads/' + upState.id + '/preview', { method: 'POST', body: { sheet } });
    upState.sheet = sheet; upState.preview = r.preview; upState.mapping = r.preview.mapping.map(c => ({ ...c })); upState.signature = r.signature;
    renderMapping(box, r);
  }

  function fieldSelect(cur, onChange) {
    const sel = h('select', { class: 'input' });
    const groups = {};
    for (const f of FIELDS.fields) (groups[f.group] = groups[f.group] || []).push(f);
    for (const [g, fs] of Object.entries(groups)) sel.append(h('optgroup', { label: g }, fs.map(f => h('option', { value: f.field }, f.label))));
    sel.value = cur; sel.addEventListener('change', () => onChange(sel.value));
    return sel;
  }

  function renderMapping(box, r) {
    stepOn(1);
    const pv = r.preview;
    const warnBox = h('ul', { class: 'warns' });
    const setWarns = ws => warnBox.replaceChildren(...(ws || []).map(w => h('li', {}, w)));
    setWarns(r.warnings);
    const rows = pv.headers.map((hd, i) => {
      const samples = pv.rows.map(row => row[i]).filter(Boolean).slice(0, 3);
      const tr = h('tr', { class: (pv.mapping[i].field && !r.remembered ? 'auto ' : '') + (upState.mapping[i].field === '' ? 'unmapped' : '') });
      tr.append(h('td', {}, hd), h('td', { class: 'samples' }, samples.length ? samples.map(s => h('span', { title: s }, s)) : h('span', {}, '(empty in sample)')),
        h('td', {}, fieldSelect(upState.mapping[i].field, v => { upState.mapping[i].field = v; tr.classList.toggle('unmapped', v === ''); tr.classList.remove('auto'); })));
      return tr;
    });
    const sheetSel = r.kind === 'xlsx' && r.sheets.length > 1 ? h('div', { class: 'field' }, h('label', {}, 'Worksheet'),
      (() => { const s = h('select', { class: 'input' }, r.sheets.map((x, i) => h('option', { value: String(i) }, x.name))); s.value = String(r.sheet); s.addEventListener('change', () => preview(box, +s.value).catch(e => toast(e.message))); return s; })()) : null;
    const info = r.kind === 'xlsx' ? 'Excel workbook' + (r.sheets[r.sheet] ? ', sheet "' + r.sheets[r.sheet].name + '"' : '') : 'CSV (' + pv.sniffed.encoding + ', delimiter "' + (pv.sniffed.delimiter === '\t' ? 'tab' : pv.sniffed.delimiter) + '"' + (pv.sniffed.gzip ? ', gzipped' : '') + ')';
    box.replaceChildren(
      h('div', { class: 'card' }, h('h2', {}, 'Map each column'),
        h('p', { class: 'hint' }, upState.preview.headers.length + ' columns · ' + info + (r.remembered ? ' · mapping remembered from a previous upload with the same headers' : ' · columns marked "auto" were recognised by name')),
        sheetSel, warnBox,
        h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid map' }, h('thead', {}, h('tr', {}, h('th', {}, 'Column in your file'), h('th', {}, 'Sample values'), h('th', {}, 'Import as'))), h('tbody', {}, rows))),
        h('p', { class: 'note mt' }, '"Keep as extra column" stores the value with the contact and includes it in CSV exports. "Ignore" drops it. Several columns may map to Email or phone fields: the first valid value in each row wins.'),
        h('div', { class: 'actions mt' }, h('button', { class: 'btn btn-primary', type: 'button', onclick: () => dryRun(box, setWarns) }, 'Check mapping on 200 rows'),
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => cancelUpload(box) }, 'Cancel'))));
  }

  async function cancelUpload(box) {
    if (upState.id) { try { await api('/api/uploads/' + upState.id, { method: 'DELETE' }); } catch { /* ignore */ } }
    renderUpStep1(box);
  }

  async function dryRun(box, setWarns) {
    let r;
    try { r = await api('/api/uploads/' + upState.id + '/dryrun', { method: 'POST', body: { sheet: upState.sheet, mapping: upState.mapping } }); }
    catch (e) { toast(e.message); return; }
    stepOn(2);
    setWarns(r.warnings);
    const sk = r.stats.reasons || {}, cl = r.stats.cleaned || {};
    const cols = ['name', 'title', 'company', 'email', 'phone', 'country', 'industry'];
    const table = h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 'Line'), cols.map(c => h('th', {}, c)), h('th', {}, 'Result'))),
      h('tbody', {}, r.rows.slice(0, 60).map(row => h('tr', {}, h('td', { class: 'num' }, row.line), cols.map(c => h('td', { class: 'wrapcell' }, (row.fields || {})[c] || '')),
        h('td', {}, row.skip ? h('span', { class: 'skip' }, 'skipped: ' + row.skip.replace('_', ' ')) : 'import', (row.notes || []).map(n => h('div', { class: 'notes' }, n)))))));
    const attest = h('input', { type: 'checkbox', id: 'attest' });
    const remember = h('input', { type: 'checkbox', id: 'remember', checked: true });
    let stores = [];
    try { stores = (await api('/api/stores')).stores; } catch { stores = []; }
    const target = h('select', { class: 'input', id: 'target' }, h('option', { value: 'auto' }, 'Auto: the first database with room (recommended)'),
      stores.filter(x => x.write && x.read).map(x => h('option', { value: String(x.id), disabled: x.state === 'down' }, x.name + ' (' + x.kind + (x.cap_bytes ? ', ' + x.pct + '% full' : '') + (x.state === 'down' ? ', unreachable' : '') + ')')));
    const go = h('button', { class: 'btn btn-primary', type: 'button' }, 'Import the whole file');
    go.addEventListener('click', async () => {
      if (!attest.checked) { toast('Please confirm you have the right to publish this data.'); return; }
      go.classList.add('loading');
      try {
        const res = await api('/api/uploads/' + upState.id + '/import', { method: 'POST', body: { sheet: upState.sheet, mapping: upState.mapping, attest: true, remember: remember.checked, signature: upState.signature, target: target.value } });
        try { sessionStorage.clear(); } catch { /* ignore */ }
        toast('Import #' + res.import_id + ' started'); location.hash = '#imports';
      } catch (e) { toast(e.message); } finally { go.classList.remove('loading'); }
    });
    box.append(h('div', { class: 'card', id: 'dry' }, h('h2', {}, 'Check: first ' + fmt(r.stats.rows) + ' rows after mapping and cleaning'),
      h('div', { class: 'stats' }, h('div', { class: 'stat' }, h('b', {}, fmt(r.stats.added)), h('span', {}, 'would import')),
        h('div', { class: 'stat' }, h('b', {}, fmt(r.stats.skipped)), h('span', {}, 'skipped (' + (Object.entries(sk).map(([k, v]) => k.replace('_', ' ') + ': ' + v).join(', ') || 'none') + ')')),
        h('div', { class: 'stat' }, h('b', {}, fmt((cl.email_invalid || 0) + (cl.email_disposable || 0))), h('span', {}, 'emails removed (invalid / disposable)')),
        h('div', { class: 'stat' }, h('b', {}, fmt(cl.phone_invalid || 0)), h('span', {}, 'phones removed (< 6 digits)'))),
      h('div', { class: 'tbl-scroll' }, table),
      h('div', { class: 'form mt' },
        h('label', { class: 'check' }, attest, h('span', {}, 'I confirm we have the right to publish these contacts, and people can remove themselves via the removal page.')),
        h('label', { class: 'check' }, remember, h('span', {}, 'Remember this mapping for files with the same columns')),
        h('div', { class: 'field' }, h('label', { for: 'target' }, 'Store new contacts in'), target,
          h('span', { class: 'hint' }, 'People already in any database are merged where they are, never duplicated. Auto moves on to the next database when one passes 90% of its quota.')),
        h('div', { class: 'actions' }, go, h('button', { class: 'btn', type: 'button', onclick: () => { $('#dry').remove(); stepOn(1); } }, 'Change mapping')))));
    $('#dry').scrollIntoView({ behavior: 'smooth' });
  }

  // ---------- imports & notify ----------
  async function vImports(alive) {
    let page = 0;
    const pagerBox = h('div');
    const r = await api('/api/imports?page=0');
    const tbody = h('tbody');
    const fill = list => tbody.replaceChildren(...list.map(u => {
      const live = u.live;
      const reasons = (() => { try { return Object.entries((u.reasons || {}).skipped || {}).map(([k, v]) => k.replace('_', ' ') + ': ' + v).join(', '); } catch { return ''; } })();
      return h('tr', {}, h('td', { class: 'num' }, '#' + u.id), h('td', { class: 'wrapcell' }, u.filename, h('div', { class: 'note' }, when(u.ts) + ' · ' + bytes(u.bytes) + ' · ' + u.by)),
        h('td', {}, h('span', { class: 'pill ' + u.status }, u.status), u.error ? h('div', { class: 'note' }, u.error) : null),
        h('td', { class: 'num' }, fmt(live ? live.rows : u.rows)), h('td', { class: 'num' }, fmt(live ? live.added : u.added)), h('td', { class: 'num' }, fmt(live ? live.updated : u.updated)),
        h('td', { class: 'num' }, fmt(u.unchanged)), h('td', { class: 'num', title: reasons }, fmt(live ? live.skipped : u.skipped)), h('td', { class: 'wrapcell note' }, u.target || '—'),
        h('td', {}, u.status === 'running' ? h('button', { class: 'btn btn-small', type: 'button', onclick: () => api('/api/imports/' + u.id + '/cancel', { method: 'POST', body: {} }).then(() => toast('Cancelling…')) }, 'Cancel')
          : u.notified ? h('span', { class: 'note' }, 'Notified (campaign #' + u.notified + ')')
            : u.status === 'done' && u.added > 0 ? h('button', { class: 'btn btn-small btn-primary', type: 'button', onclick: () => notifyDialog(u) }, 'Notify subscribers') : h('span', { class: 'note' }, '—')));
    }));
    const show = x => { fill(x.imports); pagerBox.replaceChildren(pager(x.paging, async p => { page = p; show(await api('/api/imports?page=' + p)); }) || ''); };
    show(r);
    (async () => { while (alive()) { await sleep(2500); if (!alive()) return; try { const x = await api('/api/imports?page=' + page); if (alive()) show(x); if (!x.running) { await sleep(5000); } } catch { return; } } })();
    return [head('Imports & notify', h('a', { class: 'btn btn-primary', href: '#upload' }, 'Upload data')),
      h('div', { class: 'card' }, h('p', { class: 'hint' }, 'Imports run one at a time in the background; you can leave this page. Re-importing a file never creates duplicates.'),
        h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, ['#', 'File', 'Status', 'Rows', 'New', 'Updated', 'Unchanged', 'Skipped', 'Stored in', ''].map(t => h('th', {}, t)))), tbody)), pagerBox)];
  }
  async function notifyDialog(u) {
    const ob = await api('/api/outbox');
    const subject = h('input', { class: 'input', maxlength: '150', placeholder: fmt(u.added) + ' new direct dials just added' });
    const intro = h('textarea', { class: 'input', rows: '4', maxlength: '1500', placeholder: 'We just added ' + fmt(u.added) + ' new B2B contacts with direct dials and verified emails…' });
    const dlg = h('dialog', { class: 'adm-dlg' });
    const send = h('button', { class: 'btn btn-primary', type: 'button' }, 'Queue emails');
    send.addEventListener('click', async () => {
      send.classList.add('loading');
      try {
        const r = await api('/api/imports/' + u.id + '/notify', { method: 'POST', body: { subject: subject.value, intro: intro.value } });
        dlg.close(); toast('Queued ' + fmt(r.recipients) + (r.recipients === 1 ? ' email' : ' emails') + (r.days_to_done > 1 ? ' · about ' + Math.ceil(r.days_to_done) + ' days at the current daily cap' : ''));
        route();
      } catch (e) { toast(e.message); } finally { send.classList.remove('loading'); }
    });
    dlg.append(h('div', { class: 'form' }, h('h2', {}, 'Email subscribers about upload #' + u.id),
      h('p', { class: 'hint' }, 'Sends to every subscribed, non-bounced user, within today\'s campaign share (' + fmt(ob.bulk_left_today) + ' left today); the rest continue automatically each day. Sign-in codes always keep their reserved share.'),
      h('div', { class: 'field' }, h('label', {}, 'Subject (blank = default)'), subject), h('div', { class: 'field' }, h('label', {}, 'Intro paragraph (blank = default)'), intro),
      h('div', { class: 'actions' }, send, h('button', { class: 'btn', type: 'button', onclick: () => dlg.close() }, 'Cancel'))));
    document.body.append(dlg); dlg.addEventListener('close', () => dlg.remove()); dlg.showModal();
  }

  // ---------- settings forms ----------
  const get = (o, p) => p.split('.').reduce((x, k) => x == null ? x : x[k], o);
  const set = (o, p, v) => { const ks = p.split('.'); let x = o; ks.slice(0, -1).forEach(k => { x[k] = x[k] || {}; x = x[k]; }); x[ks[ks.length - 1]] = v; };
  function input(spec, obj) {
    const id = 'f-' + spec.k.replace(/\./g, '-');
    const v = get(obj, spec.k);
    let el;
    if (spec.t === 'bool') {
      el = h('input', { type: 'checkbox', id, checked: !!v });
      el.addEventListener('change', () => set(obj, spec.k, el.checked));
      return h('label', { class: 'check' }, el, h('span', {}, spec.l, spec.hint ? h('span', { class: 'hint' }, ' ' + spec.hint) : null));
    }
    if (spec.t === 'select') {
      el = h('select', { class: 'input', id }, spec.opts.map(([val, lab]) => h('option', { value: val }, lab)));
      el.value = v == null ? spec.opts[0][0] : v;
      el.addEventListener('change', () => set(obj, spec.k, el.value));
    } else if (spec.t === 'textarea') {
      el = h('textarea', { class: 'input', id, rows: spec.rows || '4', maxlength: spec.max || '' }); el.value = v || '';
      el.addEventListener('input', () => set(obj, spec.k, el.value));
    } else {
      el = h('input', { class: 'input', id, type: spec.t === 'number' ? 'number' : spec.t === 'url' ? 'url' : 'text', min: spec.min, max: spec.max, placeholder: spec.ph || '' });
      el.value = v == null ? '' : v;
      el.addEventListener('input', () => set(obj, spec.k, spec.t === 'number' ? +el.value : el.value));
    }
    return h('div', { class: 'field' }, h('label', { for: id }, spec.l), el, spec.hint ? h('span', { class: 'hint' }, spec.hint) : null);
  }
  async function settingsView(section, title, intro, build) {
    const r = await api('/api/settings');
    const obj = JSON.parse(JSON.stringify(r.settings[section]));
    const msg = h('span', { class: 'msg' });
    const save = h('button', { class: 'btn btn-primary', type: 'button' }, 'Save');
    save.addEventListener('click', async () => {
      msg.className = 'msg'; msg.textContent = ''; save.classList.add('loading');
      try { await api('/api/settings/' + section, { method: 'POST', body: obj }); msg.className = 'msg ok'; msg.textContent = 'Saved. Live on the site within a minute.'; }
      catch (e) { msg.className = 'msg err'; msg.textContent = e.message; } finally { save.classList.remove('loading'); }
    });
    return [head(title), h('div', { class: 'card' }, intro ? h('p', { class: 'hint' }, intro) : null, h('div', { class: 'form' }, build(obj, r)), h('div', { class: 'savebar' }, save, msg))];
  }

  const PLACE_LABEL = { header: 'Header text link', sponsor_button: 'Button in the sponsor break', results: 'Text line under results', empty_states: 'Buttons in empty / all-unlocked / daily-limit states',
    slot_fallback: 'Banner in any ad slot without Adsterra code', footer: 'Footer link', seo_pages: 'SEO landing pages' };
  const SLOT_LABEL = { top: 'Top banner (728×90 / 320×50)', incontent: 'Below results (300×250 / native)', sponsor: 'Inside the sponsor break (300×250)', footer: 'Above the footer', global: 'Global (Social Bar / overlay; runs in the page)' };

  const vAds = () => settingsView('ads', 'Ads & Smart Link', 'Adsterra bans incentivized clicks. "Sponsor break" (default) shows display ads during a short timed break and never requires a click. Switch to "Smart Link" mode only if Adsterra confirms in writing that it is allowed.', (o) => {
    o.slots = o.slots || {}; o.placements = o.placements || {};
    const places = h('fieldset', {}, h('legend', {}, 'Smart Link placements (always labelled "' + (o.sponsor_label || 'Sponsored') + '", rel="sponsored", new tab)'),
      h('div', { class: 'checks' }, FIELDS.placements.map(p => { const c = h('input', { type: 'checkbox', checked: !!o.placements[p] }); c.addEventListener('change', () => { o.placements[p] = c.checked; }); return h('label', { class: 'check' }, c, h('span', {}, PLACE_LABEL[p] || p)); })));
    const slots = FIELDS.slots.map(name => {
      const s = o.slots[name] = o.slots[name] || { enabled: false, html: '', html_mobile: '', isolation: name === 'global' ? 'direct' : 'sandbox', height: 0, height_mobile: 0 };
      return h('fieldset', {}, h('legend', {}, SLOT_LABEL[name] || name),
        input({ k: 'enabled', t: 'bool', l: 'Enabled' }, s),
        input({ k: 'html', t: 'textarea', l: 'Ad code (paste from Adsterra)', max: '20000' }, s),
        name === 'global' ? null : input({ k: 'html_mobile', t: 'textarea', l: 'Mobile ad code (optional, e.g. 320×50)', max: '20000', rows: '3' }, s),
        name === 'global' ? h('p', { class: 'hint' }, 'Overlay formats run directly in the page and relax its script security policy.') :
          h('div', { class: 'row2' }, input({ k: 'isolation', t: 'select', l: 'Isolation', opts: [['sandbox', 'Sandboxed frame (safer, recommended)'], ['direct', 'Direct in page (if fill rate is low)']] }, s),
            h('div', { class: 'row2' }, input({ k: 'height', t: 'number', l: 'Height px', min: 0, max: 600 }, s), input({ k: 'height_mobile', t: 'number', l: 'Mobile px', min: 0, max: 600 }, s))),
        name === 'global' ? null : h('a', { class: 'note', href: BASE + '/ad-frame?slot=' + name, target: '_blank', rel: 'noopener' }, 'Preview saved slot ↗'));
    });
    return [
      input({ k: 'mode', t: 'select', l: 'Unlock mode', opts: [['sponsor', 'Sponsor break (recommended)'], ['vast', 'VAST video break (tags from HilltopAds, Adsterra, any network), sponsor break as fallback'], ['gpt_rewarded', 'Google rewarded video (Ad Manager), sponsor break as fallback'], ['smartlink', 'Smart Link in a new tab']] }, o),
      (() => {
        const ta = h('textarea', { class: 'input', id: 'f-vast_tags', rows: '4', spellcheck: 'false', placeholder: 'https://… one VAST tag URL per line' });
        ta.value = (o.vast_tags || []).join('\n');
        ta.addEventListener('input', () => { o.vast_tags = ta.value.split('\n').map(x => x.trim()).filter(Boolean); });
        return h('div', { class: 'field' }, h('label', { for: 'f-vast_tags' }, 'VAST tag URLs (VAST video mode; tried in order, up to 5)'), ta,
          h('span', { class: 'hint' }, 'Paste the VAST/VPAID tag URL each network gives you for a video zone (HilltopAds: Video VAST; Adsterra: VAST if your account offers it; later your Ad Manager video tag). The first that has a video plays; macros like [CACHEBUSTER] and {page_url} are filled in. Contacts unlock when the video ends; no video anywhere = the normal sponsor break.'));
      })(),
      input({ k: 'gpt_ad_unit', t: 'text', l: 'Google Ad Manager rewarded ad unit', ph: '/1234567/fdd_rewarded', hint: 'Ad Manager → Inventory → Ad units → a "Rewarded" unit. Its full path, starting with your network code. Contacts unlock the moment Google grants the reward; if Google has no video, visitors get the sponsor break instead.' }, o),
      input({ k: 'smartlink', t: 'url', l: 'Smart Link URL', hint: 'Your Adsterra Smart Link, used by every placement below.' }, o),
      h('div', { class: 'row2' }, input({ k: 'min_seconds', t: 'number', l: 'Break length (seconds)', min: 5, max: 60 }, o),
        input({ k: 'adblock', t: 'select', l: 'Ad blockers', opts: [['nudge', 'Nudge (recommended)'], ['allow', 'Allow silently'], ['require', 'Require ads to unlock']] }, o)),
      input({ k: 'probe_url', t: 'url', l: 'Ad-block probe URL (optional)', hint: 'Any script URL from your Adsterra code (e.g. the invoke.js link). If it fails to load, a blocker is on.' }, o),
      h('div', { class: 'row2' }, input({ k: 'sponsor_label', t: 'text', l: 'Disclosure label', hint: 'Required by ad and search policies; can be reworded, not removed.' }, o), input({ k: 'sponsor_cta', t: 'text', l: 'Smart Link button text' }, o)),
      h('fieldset', {}, h('legend', {}, 'Preview pages'),
        input({ k: 'preview_gate', t: 'bool', l: 'A short sponsor break opens each further page of masked rows', hint: '(page 1 is free; each later page once per visit)' }, o),
        input({ k: 'preview_seconds', t: 'number', l: 'Preview break length (seconds, 0 = off)', min: 0, max: 30 }, o)),
      places, ...slots,
      h('fieldset', {}, h('legend', {}, 'ads.txt'),
        input({ k: 'ads_txt', t: 'textarea', l: 'ads.txt lines', rows: '6', max: '20000', hint: 'Paste the lines each network gives you (Google AdSense / Ad Manager, Adsterra, Monetag…). Served at /ads.txt on this server; the main site must serve the same file at the domain root (RUNBOOK → ads.txt).' }, o),
        h('a', { class: 'note', href: '/ads.txt', target: '_blank', rel: 'noopener' }, 'Open /ads.txt ↗'))];
  });

  const AFF_LABEL = { results: 'Under the results', dock: 'In the unlock box', empty: 'All-unlocked, daily-limit and no-results states', break: 'During sponsor breaks', footer: 'Footer link', seo: 'SEO landing pages' };
  const vAffiliates = () => settingsView('affiliates', 'Partner offers (affiliate links)', 'Your affiliate links (Apollo, Lusha, Hunter…) shown as clearly labelled partner cards. Paste the tracking link from each programme; the most engaged spots are the all-unlocked and daily-limit states, where people want more data.', o => {
    o.items = o.items || [];
    const list = h('div', { class: 'form' });
    const render = () => list.replaceChildren(...o.items.map((it, i) => {
      it.placements = it.placements || [];
      const pl = h('div', { class: 'checks' }, (FIELDS.aff_placements || []).map(p => {
        const c = h('input', { type: 'checkbox', checked: it.placements.includes(p) });
        c.addEventListener('change', () => { it.placements = c.checked ? [...new Set([...it.placements, p])] : it.placements.filter(x => x !== p); });
        return h('label', { class: 'check' }, c, h('span', {}, AFF_LABEL[p] || p));
      }));
      return h('div', { class: 'wallet' }, h('div', { class: 'row2' }, input({ k: 'name', t: 'text', l: 'Brand' }, it), input({ k: 'cta', t: 'text', l: 'Button text' }, it)),
        input({ k: 'text', t: 'text', l: 'One-line pitch' }, it), input({ k: 'url', t: 'url', l: 'Your affiliate / tracking link (https)' }, it),
        h('div', { class: 'field' }, h('span', { class: 'label' }, 'Show in'), pl),
        h('div', { class: 'row2' }, input({ k: 'weight', t: 'number', l: 'Weight (1–100, how often when several fit)', min: 1, max: 100 }, it), input({ k: 'enabled', t: 'bool', l: 'Live on the site' }, it)),
        h('div', {}, h('button', { class: 'btn btn-small', type: 'button', onclick: () => { o.items.splice(i, 1); render(); } }, 'Remove offer')));
    }), h('div', {}, h('button', { class: 'btn btn-small', type: 'button', onclick: () => { o.items.push({ name: '', text: '', cta: 'Learn more', url: 'https://', placements: ['empty'], weight: 50, enabled: false }); render(); } }, 'Add partner offer')));
    render();
    return [input({ k: 'enabled', t: 'bool', l: 'Show partner offers' }, o), input({ k: 'label', t: 'text', l: 'Disclosure label', hint: 'Shown on every card (FTC / ASCI). Can be reworded, not removed.' }, o), list];
  });

  const vDonations = () => settingsView('donations', 'Donations', 'Each method appears on the site only when enabled and filled in. Crypto addresses are checksum-validated (EIP-55) so a typo can\'t lose funds.', (o, r) => {
    o.upi = o.upi || {}; o.paypal = o.paypal || {}; o.link = o.link || {}; o.wallets = o.wallets || [];
    const wallets = h('div', { class: 'form' });
    const renderW = () => wallets.replaceChildren(...o.wallets.map((w, i) => {
      w.networks = w.networks || [];
      const nets = h('div', { class: 'checks' });
      const fillNets = () => nets.replaceChildren(...FIELDS.networks.filter(n => n.kind === (w.kind || 'evm')).map(n => {
        const c = h('input', { type: 'checkbox', checked: w.networks.includes(n.key) });
        c.addEventListener('change', () => { w.networks = c.checked ? [...new Set([...w.networks, n.key])] : w.networks.filter(x => x !== n.key); });
        return h('label', { class: 'check' }, c, h('span', {}, n.name));
      }));
      fillNets();
      const kind = input({ k: 'kind', t: 'select', l: 'Type', opts: [['evm', 'EVM (Ethereum, BNB, Polygon, Base…)'], ['btc', 'Bitcoin'], ['sol', 'Solana'], ['tron', 'TRON']] }, w);
      $('select', kind).addEventListener('change', () => { w.networks = []; fillNets(); });
      const pub = (r.public.donations.wallets || []).find(x => x.id === w.id);
      const qr = pub && pub.qr ? (() => { const d = h('div', { class: 'qr-prev', role: 'img', 'aria-label': 'Saved QR' }); d.innerHTML = pub.qr; return d; })() : null;
      return h('div', { class: 'wallet' }, h('div', { class: 'row2' }, input({ k: 'label', t: 'text', l: 'Label' }, w), kind),
        input({ k: 'address', t: 'text', l: 'Address' }, w), h('div', { class: 'field' }, h('span', { class: 'label' }, 'Networks'), nets),
        input({ k: 'tokens', t: 'text', l: 'Tokens accepted', ph: 'ETH, USDT, USDC' }, w), input({ k: 'enabled', t: 'bool', l: 'Show on site' }, w), qr,
        h('div', {}, h('button', { class: 'btn btn-small', type: 'button', onclick: () => { o.wallets.splice(i, 1); renderW(); } }, 'Remove wallet')));
    }), h('div', {}, h('button', { class: 'btn btn-small', type: 'button', onclick: () => { o.wallets.push({ kind: 'evm', address: '', networks: [], tokens: '', label: 'Crypto', enabled: true }); renderW(); } }, 'Add wallet')));
    renderW();
    return [input({ k: 'enabled', t: 'bool', l: 'Show the donation section' }, o), input({ k: 'heading', t: 'text', l: 'Heading' }, o), input({ k: 'message', t: 'textarea', l: 'Message', rows: '2' }, o),
      h('fieldset', {}, h('legend', {}, 'UPI'), input({ k: 'enabled', t: 'bool', l: 'Enabled' }, o.upi), h('div', { class: 'row2' }, input({ k: 'vpa', t: 'text', l: 'UPI ID', ph: 'name@okicici' }, o.upi), input({ k: 'name', t: 'text', l: 'Payee name' }, o.upi))),
      h('fieldset', {}, h('legend', {}, 'PayPal'), input({ k: 'enabled', t: 'bool', l: 'Enabled' }, o.paypal), input({ k: 'url', t: 'text', l: 'paypal.me handle or link', ph: 'https://paypal.me/yourname' }, o.paypal)),
      h('fieldset', {}, h('legend', {}, 'Other link (Buy Me a Coffee, Ko-fi…)'), input({ k: 'enabled', t: 'bool', l: 'Enabled' }, o.link), h('div', { class: 'row2' }, input({ k: 'label', t: 'text', l: 'Label' }, o.link), input({ k: 'url', t: 'url', l: 'URL' }, o.link))),
      h('fieldset', {}, h('legend', {}, 'Crypto wallets'), wallets)];
  });

  const vWhatsApp = () => settingsView('whatsapp', 'WhatsApp (bulk / custom data)', 'The button pre-fills your message plus the visitor\'s current filters and match count, so leads arrive qualified.', o => {
    const test = h('a', { class: 'btn btn-small', target: '_blank', rel: 'noopener', href: '#' }, 'Test the link');
    test.addEventListener('mouseenter', () => { test.href = 'https://wa.me/' + String(o.number || '').replace(/\D/g, '') + '?text=' + encodeURIComponent(o.message || ''); });
    return [input({ k: 'enabled', t: 'bool', l: 'Show WhatsApp buttons' }, o), input({ k: 'number', t: 'text', l: 'WhatsApp number with country code', ph: '91 98765 43210' }, o),
      input({ k: 'label', t: 'text', l: 'Header button text' }, o), input({ k: 'message', t: 'textarea', l: 'Pre-filled message', rows: '3' }, o), h('div', {}, test)];
  });

  const vLimits = () => settingsView('limits', 'Limits', 'Fair use and anti-scraping. Daily limits reset at midnight UTC.', o => [
    h('div', { class: 'row2' }, input({ k: 'contacts_per_ad', t: 'number', l: 'Contacts per sponsor break', min: 1, max: 50 }, o), input({ k: 'max_ads_per_day', t: 'number', l: 'Breaks per user per day', min: 1, max: 1000 }, o)),
    h('div', { class: 'row2' }, input({ k: 'max_ads_per_day_ip', t: 'number', l: 'Breaks per network (IP) per day', min: 1, max: 10000 }, o), input({ k: 'page_size', t: 'number', l: 'Unlocked rows per page (signed-in)', min: 10, max: 200 }, o)),
    input({ k: 'preview_pages', t: 'number', l: 'Free preview pages per filter (10 masked rows each)', min: 1, max: 10 }, o),
    input({ k: 'signups_enabled', t: 'bool', l: 'Allow new sign-ups' }, o), input({ k: 'require_business_email', t: 'bool', l: 'Business emails only', hint: '(blocks Gmail, Yahoo, Outlook…)' }, o),
    input({ k: 'use_reacher', t: 'bool', l: 'Verify mailboxes with Reacher', hint: '(needs REACHER_URL on the server)' }, o)]);

  const vSite = () => settingsView('site', 'Site & legal', 'Your legal name and postal address appear in every email footer (required by anti-spam laws).', o => [
    input({ k: 'announcement', t: 'textarea', l: 'Announcement bar (blank = hidden)', rows: '2' }, o),
    input({ k: 'read_only', t: 'bool', l: 'Maintenance mode', hint: '(search keeps working; sign-ups and unlocks pause)' }, o),
    h('div', { class: 'row2' }, input({ k: 'legal_name', t: 'text', l: 'Legal / company name' }, o), input({ k: 'contact_email', t: 'text', l: 'Privacy contact email' }, o)),
    input({ k: 'address', t: 'textarea', l: 'Postal address', rows: '2' }, o),
    input({ k: 'consent_mode', t: 'select', l: 'Cookie consent banner', opts: [['eu_only', 'EU / UK visitors only (recommended)'], ['all', 'Everyone'], ['off', 'Off']] }, o),
    input({ k: 'subscribe_default', t: 'bool', l: 'Pre-tick "email me when new contacts are added" at sign-up' }, o)]);

  // ---------- email ----------
  async function vEmail() {
    const ob = await api('/api/outbox');
    const to = h('input', { class: 'input', type: 'email', placeholder: 'you@company.com' });
    const tbtn = h('button', { class: 'btn', type: 'button' }, 'Send test email');
    tbtn.addEventListener('click', async () => { try { const r = await api('/api/test-email', { method: 'POST', body: { to: to.value } }); toast('Sent via ' + r.provider); } catch (e) { toast(e.message); } });
    const pause = h('button', { class: 'btn', type: 'button' }, ob.paused ? 'Resume sending' : 'Pause sending');
    pause.addEventListener('click', async () => { await api('/api/outbox/pause', { method: 'POST', body: { paused: !ob.paused } }); route(); });
    return [head('Email'),
      h('div', { class: 'stats' }, stat(fmt(ob.queued), 'queued'), stat(fmt(ob.sent), 'sent (7 days)'), stat(fmt(ob.failed), 'failed'), stat(fmt(ob.bulk_left_today), 'campaign emails left today'),
        stat(ob.days_to_done ? ob.days_to_done.toFixed(1) : '0', 'days to finish queue'), stat(ob.resume_at ? new Date(ob.resume_at * 1000).toLocaleTimeString() : 'now', 'resumes at')),
      h('div', { class: 'card' }, h('h2', {}, 'Daily quota'), usageTable(ob), h('p', { class: 'hint mt' }, 'Campaigns stop at their share of each cap so sign-in codes always go through. Add providers or raise caps with MAIL_PROVIDERS in dials.env (e.g. brevo:300,resend:100,smtp:5000).'), h('div', { class: 'actions mt' }, pause)),
      h('div', { class: 'card' }, h('h2', {}, 'Test delivery'), h('div', { class: 'grid2' }, to, tbtn))];
  }

  // ---------- removals, blocklist, users, audit, system ----------
  async function vRemovals() {
    const [pend, done] = await Promise.all([api('/api/removals?status=pending'), api('/api/removals?status=done')]);
    pendingRemovals = (pend.paging || {}).total || pend.removals.length;
    const doneBox = h('div');
    const kind = h('select', { class: 'input' }, [['email', 'Email'], ['linkedin', 'LinkedIn URL'], ['phone', 'Phone']].map(([v, l]) => h('option', { value: v }, l)));
    const val = h('input', { class: 'input', placeholder: 'Value to remove' });
    const rm = h('button', { class: 'btn btn-primary', type: 'button' }, 'Delete & block');
    rm.addEventListener('click', async () => { try { const r = await api('/api/contacts/remove', { method: 'POST', body: { kind: kind.value, value: val.value } }); toast(r.removed + ' record(s) deleted and blocked'); val.value = ''; } catch (e) { toast(e.message); } });
    const act = (r, action) => async () => {
      const body = { action };
      if (action === 'delete' && r.kind === 'other') { const k = prompt('Identify by: email, linkedin or phone?', 'email'); if (!k) return; body.kind = k; body.value = prompt('Value to remove', r.value) || ''; }
      try { const x = await api('/api/removals/' + r.id, { method: 'POST', body }); toast(action === 'delete' ? (x.removed + ' record(s) deleted') : 'Rejected'); route(); } catch (e) { toast(e.message); }
    };
    const rows = list => list.map(r => h('tr', {}, h('td', { class: 'num' }, '#' + r.id), h('td', {}, r.kind), h('td', { class: 'wrapcell' }, r.value), h('td', { class: 'wrapcell' }, r.note),
      h('td', {}, when(r.ts)), h('td', {}, r.status === 'pending' ? [h('div', { class: 'note' }, 'due ' + new Date(r.due * 1000).toLocaleDateString()),
        h('div', { class: 'actions' }, h('button', { class: 'btn btn-small btn-primary', type: 'button', onclick: act(r, 'delete') }, 'Delete'), h('button', { class: 'btn btn-small', type: 'button', onclick: act(r, 'reject') }, 'Reject'))] : h('span', { class: 'pill ' + r.status }, r.status))));
    const tbl = list => h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, ['#', 'Type', 'Value', 'Note', 'Received', ''].map(t => h('th', {}, t)))), h('tbody', {}, rows(list))));
    return [head('Removals'), h('div', { class: 'card' }, h('h2', {}, 'Waiting (' + pend.removals.length + ')'), h('p', { class: 'hint' }, 'Requests from people without access to the listed email. Legal deadline: 30 days. Self-service email removals happen instantly and appear under Done.'), pend.removals.length ? tbl(pend.removals) : h('p', { class: 'muted' }, 'Nothing waiting.')),
      h('div', { class: 'card' }, h('h2', {}, 'Remove a contact now'), h('div', { class: 'row2' }, kind, val), h('div', { class: 'actions mt' }, rm)),
      (() => { const show = x => doneBox.replaceChildren(tbl(x.removals), pager(x.paging, async p => show(await api('/api/removals?status=done&page=' + p))) || ''); show(done);
        return h('div', { class: 'card' }, h('h2', {}, 'Done'), doneBox); })()];
  }

  async function vBlocklist() {
    const q = h('input', { class: 'input', type: 'search', placeholder: 'Search domains' });
    const list = h('tbody');
    const load = async () => {
      const r = await api('/api/blocklist?q=' + encodeURIComponent(q.value));
      info.textContent = fmt(r.builtin) + ' built-in domains + ' + fmt(r.admin) + ' added by you. Subdomains and name patterns (tempmail, mailinator, guerrilla…) are always blocked.';
      list.replaceChildren(...r.domains.map(d => h('tr', {}, h('td', {}, d.domain), h('td', {}, d.source), h('td', {}, h('button', { class: 'btn btn-small', type: 'button', onclick: async () => { await api('/api/blocklist/' + encodeURIComponent(d.domain), { method: 'DELETE' }); load(); } }, 'Remove')))));
    };
    const info = h('p', { class: 'hint' });
    const ta = h('textarea', { class: 'input', rows: '5', placeholder: 'One domain per line (or comma separated)' });
    const add = h('button', { class: 'btn btn-primary', type: 'button' }, 'Add domains');
    add.addEventListener('click', async () => { try { const r = await api('/api/blocklist', { method: 'POST', body: { domains: ta.value } }); toast(r.added + ' added' + (r.invalid.length ? ' · invalid: ' + r.invalid.join(', ') : '')); ta.value = ''; load(); } catch (e) { toast(e.message); } });
    let t; q.addEventListener('input', () => { clearTimeout(t); t = setTimeout(load, 250); });
    await load();
    return [head('Disposable-email blocklist'), h('div', { class: 'card' }, info, h('div', { class: 'form' }, ta, h('div', {}, add))),
      h('div', { class: 'card' }, q, h('div', { class: 'tbl-scroll mt' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, h('th', {}, 'Domain'), h('th', {}, 'Source'), h('th', {}))), list)))];
  }

  async function vUsers() {
    const q = h('input', { class: 'input', type: 'search', placeholder: 'Search by email' });
    const list = h('tbody');
    const pagerBox = h('div');
    let page = 0;
    const load = async () => {
      const r = await api('/api/users?q=' + encodeURIComponent(q.value) + '&page=' + page);
      pagerBox.replaceChildren(pager(r.paging, p => { page = p; load(); }) || '');
      list.replaceChildren(...r.users.map(u => h('tr', {}, h('td', { class: 'wrapcell' }, u.email), h('td', {}, when(u.created)), h('td', {}, when(u.last_seen)), h('td', { class: 'num' }, fmt(u.breaks)),
        h('td', {}, [u.subscribed ? 'subscribed' : 'unsubscribed', u.bounced ? 'bounced' : '', u.blocked ? 'BLOCKED' : ''].filter(Boolean).join(' · ')),
        h('td', {}, h('div', { class: 'actions' },
          h('button', { class: 'btn btn-small', type: 'button', onclick: async () => { await api('/api/users/' + u.id + '/block', { method: 'POST', body: { blocked: !u.blocked } }); load(); } }, u.blocked ? 'Unblock' : 'Block'),
          h('button', { class: 'btn btn-small', type: 'button', onclick: async () => { if (!confirm('Permanently delete ' + u.email + ' and their unlock history?')) return; try { await api('/api/users/' + u.id, { method: 'DELETE' }); load(); } catch (e) { toast(e.message); } } }, 'Delete'))))));
    };
    let t; q.addEventListener('input', () => { clearTimeout(t); page = 0; t = setTimeout(load, 250); });
    await load();
    return [head('Users'), h('div', { class: 'card' }, q, h('div', { class: 'tbl-scroll mt' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, ['Email', 'Joined', 'Last seen', 'Sponsor breaks', 'Status', ''].map(x => h('th', {}, x)))), list)), pagerBox)];
  }

  async function vAudit() {
    const body = h('tbody'), more = h('div', { class: 'actions mt' });
    const load = async before => {
      const r = await api('/api/audit' + (before ? '?before=' + before : ''));
      body.append(...r.audit.map(a => h('tr', {}, h('td', {}, when(a.ts)), h('td', {}, a.actor), h('td', {}, a.ip), h('td', {}, a.action), h('td', { class: 'wrapcell' }, a.detail))));
      more.replaceChildren(r.next ? h('button', { class: 'btn btn-small', type: 'button', onclick: () => load(r.next) }, 'Load older entries') : h('span', { class: 'note' }, 'Start of the log.'));
    };
    await load(0);
    return [head('Audit log'), h('div', { class: 'card' }, h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, ['When', 'Who', 'IP', 'Action', 'Detail'].map(x => h('th', {}, x)))), body)), more)];
  }

  // ---------- contact databases (built-in + Supabase / Postgres) ----------
  function storesTable(stores, manage, reload) {
    return h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, ['#', 'Database', 'State', 'Contacts', 'Size', 'Quota', 'Last check', manage ? '' : null].filter(x => x !== null).map(x => h('th', {}, x)))),
      h('tbody', {}, stores.map(x => h('tr', {},
        h('td', { class: 'num' }, x.id), h('td', { class: 'wrapcell' }, h('b', {}, x.name), h('div', { class: 'note' }, x.kind === 'local' ? 'SQLite on this server' : x.kind + (x.host ? ' · ' + x.host : ''))),
        h('td', {}, h('span', { class: 'pill ' + (x.state === 'ok' ? 'done' : x.state === 'down' ? 'failed' : 'pending') }, x.state), x.last_err ? h('div', { class: 'note' }, x.last_err) : null),
        h('td', { class: 'num' }, fmt(x.contacts)), h('td', { class: 'num' }, bytes(x.bytes)),
        h('td', { class: 'num' }, x.cap_bytes ? x.pct + '% of ' + bytes(x.cap_bytes) : '—'), h('td', {}, when(x.last_ok)),
        manage ? h('td', {}, storeActions(x, reload)) : null)))));
  }
  function storeActions(x, reload) {
    const quota = h('input', { class: 'input', type: 'number', min: 0, value: String(Math.round(x.cap_bytes / (1 << 20))), 'aria-label': 'Quota MB' });
    const name = h('input', { class: 'input', value: x.name, 'aria-label': 'Name' });
    const rd = h('input', { type: 'checkbox', checked: x.read, disabled: x.id === 0 }), wr = h('input', { type: 'checkbox', checked: x.write });
    const save = h('button', { class: 'btn btn-small btn-primary', type: 'button' }, 'Save');
    save.addEventListener('click', async () => {
      try { await api('/api/stores/' + x.id, { method: 'POST', body: { name: name.value, read: rd.checked, write: wr.checked, quota_mb: +quota.value || 0 } }); toast('Saved'); reload(); } catch (e) { toast(e.message); }
    });
    const detach = x.id === 0 ? null : h('button', { class: 'btn btn-small', type: 'button', onclick: async () => {
      if (!confirm('Detach "' + x.name + '"? Its data stays in that database; adding the same connection string later re-attaches it.')) return;
      try { await api('/api/stores/' + x.id, { method: 'DELETE' }); toast('Detached'); reload(); } catch (e) { toast(e.message); }
    } }, 'Detach');
    return h('div', { class: 'form' }, name, h('label', { class: 'check' }, rd, h('span', {}, 'Serve searches')), h('label', { class: 'check' }, wr, h('span', {}, 'Receive uploads')),
      x.id === 0 ? h('span', { class: 'hint' }, 'Quota: managed by the disk guard') : h('div', { class: 'field' }, h('label', {}, 'Quota (MB)'), quota), h('div', { class: 'actions' }, save, detach));
  }
  async function vDatabases() {
    const box = h('div');
    const load = async () => { const r = await api('/api/stores'); box.replaceChildren(storesTable(r.stores, true, load)); };
    await load();
    const name = h('input', { class: 'input', id: 'db-name', placeholder: 'Supabase 1 (Mumbai)' });
    const dsn = h('input', { class: 'input', id: 'db-dsn', type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: 'postgresql://postgres.xxxx:PASSWORD@aws-0-ap-south-1.pooler.supabase.com:5432/postgres' });
    const quota = h('input', { class: 'input', id: 'db-quota', type: 'number', min: 0, value: '500' });
    const out = h('div', { class: 'hint' });
    const test = h('button', { class: 'btn', type: 'button' }, 'Test connection');
    const add = h('button', { class: 'btn btn-primary', type: 'button' }, 'Add database');
    test.addEventListener('click', async () => {
      out.textContent = 'Connecting…'; test.classList.add('loading');
      try { const p = await api('/api/stores/test', { method: 'POST', body: { kind: 'postgres', dsn: dsn.value } });
        out.textContent = 'Connected in ' + p.latency_ms + ' ms · ' + p.server + ' · ' + bytes(p.bytes) + ' used' + (p.has_schema ? ' · already has Free Direct Dials data (' + fmt(p.contacts) + ' contacts): adding it re-attaches them' : ' · empty: the tables are created when you add it'); }
      catch (e) { out.textContent = e.message; } finally { test.classList.remove('loading'); }
    });
    add.addEventListener('click', async () => {
      add.classList.add('loading');
      try { await api('/api/stores', { method: 'POST', body: { name: name.value, kind: 'postgres', dsn: dsn.value, quota_mb: +quota.value || 0 } });
        toast('Database added'); dsn.value = ''; name.value = ''; out.textContent = ''; load(); }
      catch (e) { out.textContent = e.message; } finally { add.classList.remove('loading'); }
    });
    const check = h('button', { class: 'btn', type: 'button', onclick: async () => { try { const r = await api('/api/stores/check', { method: 'POST', body: {} }); toast(r.warnings.length ? r.warnings.join(' · ') : 'Every database answered'); load(); } catch (e) { toast(e.message); } } }, 'Ping all now');
    return [head('Contact databases', check),
      h('div', { class: 'card' }, h('p', { class: 'hint' }, 'Searches ask this server first, then each database in the order added, and only when needed: totals, single filters and plain dropdowns come from a local summary. When one database has no more matches, the next is searched in the background. Every database is pinged every 6 hours so free Supabase projects never pause.'), box),
      h('div', { class: 'card' }, h('h2', {}, 'Add a Supabase / Postgres database'),
        h('ol', { class: 'hint' }, h('li', {}, 'In Supabase: Project → Connect → "Session pooler" → copy the URI (it works over IPv4).'),
          h('li', {}, 'Replace [YOUR-PASSWORD] with the database password (Project Settings → Database).'),
          h('li', {}, 'Paste it below, Test, then Add. The tables are created in their own schema (fdd), invisible to Supabase\'s public APIs.')),
        h('div', { class: 'form' }, h('div', { class: 'row2' }, h('div', { class: 'field' }, h('label', { for: 'db-name' }, 'Name'), name), h('div', { class: 'field' }, h('label', { for: 'db-quota' }, 'Quota (MB)'), quota, h('span', { class: 'hint' }, 'Free plan: 500 MB. New uploads move on at 90%.'))),
          h('div', { class: 'field' }, h('label', { for: 'db-dsn' }, 'Connection string'), dsn, h('span', { class: 'hint' }, 'Stored encrypted (AES-256-GCM) on this server; never shown again or sent to the browser.')),
          h('div', { class: 'actions' }, test, add), out)),
      h('div', { class: 'card' }, h('h2', {}, 'Good to know'), h('ul', { class: 'hint' },
        h('li', {}, 'Supabase allows two free projects per account. Using many accounts to get more breaks their terms; upgrade one project (Pro: 8 GB) when you need more room.'),
        h('li', {}, 'Removals and uploads need every database online, so nobody is ever duplicated or left behind.'),
        h('li', {}, 'A down database is skipped automatically (visitors see the rest) and retried; you get an email alert.')))];
  }

  async function vSystem() {
    const s = await api('/api/stats');
    return [head('System'),
      h('div', { class: 'stats' }, stat(s.disk.pct.toFixed(1) + '%', 'disk used (level ' + s.disk.level + ')'), stat(bytes(s.disk.free), 'disk free'), stat(bytes(s.db.bytes), 'database'), stat(bytes(s.db.wal_bytes), 'WAL'),
        stat('v' + s.db.dataset_version, 'dataset version'), stat(s.runtime.heap_mb + ' / ' + s.runtime.sys_mb + ' MB', 'heap / process'), stat(fmt(s.runtime.goroutines), 'goroutines'), stat(Math.round(s.runtime.uptime_s / 3600) + ' h', 'uptime'),
        stat(fmt(s.cache.entries), 'cached responses'), stat(s.cache.hits + s.cache.misses ? Math.round(100 * s.cache.hits / (s.cache.hits + s.cache.misses)) + '%' : '—', 'cache hit rate')),
      h('div', { class: 'card' }, h('h2', {}, 'Background jobs'), h('div', { class: 'tbl-scroll' }, h('table', { class: 'grid' }, h('thead', {}, h('tr', {}, ['Job', 'Last run', 'Result', 'Next', ''].map(x => h('th', {}, x)))),
        h('tbody', {}, s.jobs.map(j => h('tr', {}, h('td', {}, j.name), h('td', {}, when(j.started)), h('td', { class: 'wrapcell' }, h('span', { class: 'pill ' + (j.running ? 'running' : j.ok ? 'done' : j.started ? 'failed' : '') }, j.running ? 'running' : j.ok ? 'ok' : j.started ? 'failed' : 'not yet'), ' ', j.msg),
          h('td', {}, when(j.next)), h('td', {}, h('button', { class: 'btn btn-small', type: 'button', onclick: () => runJob(j.name) }, 'Run now')))))))),
      h('div', { class: 'card' }, h('h2', {}, 'Backups (kept: 2 newest)'), s.backups && s.backups.length ? h('ul', {}, s.backups.map(b => h('li', {}, b.name + ' · ' + bytes(b.bytes) + ' · ' + when(b.time)))) : h('p', { class: 'muted' }, 'No backups yet.'),
        h('p', { class: 'hint mt' }, 'Copy backups off the server too (RUNBOOK.md → Offsite backups).'), h('div', { class: 'actions mt' }, h('button', { class: 'btn', type: 'button', onclick: () => runJob('backup') }, 'Back up now'), h('button', { class: 'btn', type: 'button', onclick: purge }, 'Purge caches')))];
  }

  const VIEW_FN = { dashboard: vDashboard, upload: vUpload, imports: vImports, databases: vDatabases, ads: vAds, affiliates: vAffiliates, donations: vDonations, whatsapp: vWhatsApp, limits: vLimits, site: vSite,
    email: vEmail, removals: vRemovals, blocklist: vBlocklist, users: vUsers, audit: vAudit, system: vSystem };

  (async () => {
    try { const s = await api('/api/session'); CSRF = s.csrf; USER = s.username; start(); }
    catch { renderLogin(); }
  })();
})();
