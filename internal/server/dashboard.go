package server

// dashboardHTML is the embedded dashboard: health + memory stats (gated by
// the admin password), cumulative totals, persisted per provider+model
// rollups (source=store), admin password persisted in localStorage. No
// external assets.
const dashboardHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>onegw</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; margin: 2rem; }
  h1 { font-size: 1.2rem; }
  h1 .ver { font-size: .75rem; opacity: .5; font-weight: normal; }
  table { border-collapse: collapse; margin-top: 1rem; min-width: 760px; }
  th, td { border: 1px solid #8884; padding: 4px 10px; text-align: right; }
  th:first-child, td:first-child { text-align: left; }
  .muted { opacity: .6; }
  .err { color: #c0392b; }
  input, select { font: inherit; padding: 2px 6px; }
  #stamp { font-size: .75rem; opacity: .5; }
</style>
</head>
<body>
<h1>onegw <span class="ver">v0.1</span> <span class="muted">LLM gateway</span></h1>
<p>
  <span id="health">checking…</span> · live <b id="inflight">-</b>
  · uptime <span id="uptime" class="muted">-</span>
  · heap <span id="heap" class="muted">-</span> · sys <span id="sys" class="muted">-</span>
  <span id="stamp" class="muted"></span>
</p>
<p class="muted">Endpoints: /v1/chat/completions · /v1/messages · /v1beta/models/{m}:generateContent · /v1/models</p>
<p>
  range: <select id="range" onchange="setRange(this.value)">
    <option value="today">today</option>
    <option value="7d">7 days</option>
    <option value="1m">1 month</option>
    <option value="all">all time</option>
  </select>
</p>
<p id="totals">usage (since process start): <span class="muted">enter admin password to view</span></p>
<p id="quota" class="muted"></p>
<p>
  admin password: <input id="pw" type="password" placeholder="(admin_password from config)" size="28">
  <button onclick="savePw()">save</button> <span id="authstate" class="err"></span>
</p>
<table id="usage">
  <thead><tr><th>provider</th><th>model</th><th>reqs</th><th>in tok</th><th>out tok</th><th>cache read</th><th>saved</th></tr></thead>
  <tbody></tbody>
</table>
<script>
const pwInput = document.getElementById('pw');
pwInput.value = localStorage.getItem('onegw_admin') || '';
function savePw() { localStorage.setItem('onegw_admin', pwInput.value); refresh(); }
function authHeaders() {
  const pw = localStorage.getItem('onegw_admin') || '';
  return pw ? { 'X-Admin-Password': pw } : {};
}
function fmtCompact(n) { const t = n >= 1e9 ? [n/1e9, 'B'] : n >= 1e6 ? [n/1e6, 'M'] : n >= 1e3 ? [n/1e3, 'K'] : null; return t ? t[0].toFixed(1).replace(/\.0$/, '') + t[1] : String(n); }
// Range filter: days is the coarse UTC fetch window (server caps < 366),
// back the exact browser-local day cutoff (-1 = no cutoff). Selection
// lives in ?range= so it survives a reload; unknown/missing = all time.
const RANGES = {
  today: { days: 2, back: 0, label: 'today' },
  '7d': { days: 8, back: 6, label: '7 days' },
  '1m': { days: 32, back: 29, label: '1 month' },
  all: { days: 365, back: -1, label: 'all time' },
};
function currentRange() {
  const q = new URLSearchParams(location.search).get('range');
  return RANGES[q] ? q : 'all';
}
function setRange(r) {
  const url = new URL(location.href);
  url.searchParams.set('range', r);
  history.replaceState(null, '', url);
  refresh();
}
async function refresh() {
  try {
    const hr = await fetch('/admin/health', { headers: authHeaders() });
    if (hr.status === 401) {
      document.getElementById('health').textContent = 'unauthorized';
      document.getElementById('health').className = 'err';
    } else {
      const h = await hr.json();
      document.getElementById('health').textContent = 'ok';
      document.getElementById('health').className = '';
      document.getElementById('uptime').textContent = h.uptime_s + 's';
      document.getElementById('inflight').textContent = h.inflight ?? '-';
      document.getElementById('heap').textContent = h.heap_alloc_mb + ' MiB';
      document.getElementById('sys').textContent = h.sys_mb + ' MiB';
      document.getElementById('stamp').textContent = '· updated ' + new Date().toLocaleTimeString();
    }
  } catch (e) {
    document.getElementById('health').textContent = 'error';
    document.getElementById('health').className = 'err';
  }
  try {
    const key = currentRange();
    const range = RANGES[key];
    document.getElementById('range').value = key;
    // Coarse UTC fetch wide enough for any timezone; the exact browser-local
    // cutoff happens below (store rows carry UTC day+hour).
    const resp = await fetch('/admin/usage?source=store&days=' + range.days, { headers: authHeaders() });
    if (resp.status === 401) {
      document.getElementById('authstate').textContent = 'unauthorized — enter password';
      return;
    }
    document.getElementById('authstate').textContent = '';
    const u = await resp.json();
    // Keep rows inside the viewer's local window so the numbers match the
    // user's timezone, not UTC. back < 0 (all time) skips the cutoff.
    let cutoff = 0;
    if (range.back >= 0) {
      const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - range.back);
      cutoff = d.getTime();
    }
    const win = (u.rows || []).filter(r => !cutoff || Date.UTC(
      +r.day.slice(0, 4), +r.day.slice(5, 7) - 1, +r.day.slice(8, 10), +(r.hour || 0)) >= cutoff);
    // Totals and the table are computed from the same filtered rows, so the
    // header can never disagree with the table.
    const t = { requests: 0, input: 0, output: 0, saved: 0 };
    for (const r of win) {
      t.requests += r.requests || 0; t.input += r.input || 0;
      t.output += r.output || 0; t.saved += r.saved || 0;
    }
    document.getElementById('totals').innerHTML =
      '<b>' + t.requests + '</b> reqs · in <b>' + fmtCompact(t.input) +
      '</b> tok · out <b>' + fmtCompact(t.output) + '</b> tok · sum <b>' + fmtCompact(t.input + t.output) +
      '</b> tok · saved <b>' + fmtCompact(t.saved) +
      '</b> tok <span class="muted">(' + range.label + ', your local time; table = same window per provider+model)</span>';
    // Aggregate the filtered rows into provider+model totals.
    const agg = {};
    for (const r of win) {
      const k = r.provider + '\u0000' + r.model;
      const a = agg[k] || (agg[k] = { provider: r.provider, model: r.model, requests: 0, input: 0, output: 0, cacheRead: 0, saved: 0 });
      a.requests += r.requests || 0; a.input += r.input || 0; a.output += r.output || 0;
      a.cacheRead += r.cacheRead || 0; a.saved += r.saved || 0;
    }
    const rows = Object.values(agg);
    rows.sort((a, b) => (b.input + b.output) - (a.input + a.output));
    const tb = document.querySelector('#usage tbody');
    tb.innerHTML = '';
    for (const b of rows) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td>' + b.provider + '</td><td>' + b.model + '</td><td>' + b.requests +
        '</td><td>' + fmtCompact(b.input) + '</td><td>' + fmtCompact(b.output) + '</td><td>' + fmtCompact(b.cacheRead) + '</td><td>' + fmtCompact(b.saved) + '</td>';
      tb.appendChild(tr);
    }
  } catch (e) { /* usage table still shown */ }
}
async function quotaRefresh() {
  try {
    const qr = await fetch('/admin/quota', { headers: authHeaders() });
    if (qr.ok) {
      const q = await qr.json();
      const bits = (q.providers || []).map(s => {
        const reset = Math.max(0, Math.round((new Date(s.window_end) - Date.now()) / 1000));
        const h = Math.floor(reset / 3600), m = Math.floor((reset % 3600) / 60);
        const cnt = 'reset ' + (h > 0 ? h + 'h' + (m ? m + 'm' : '') : m + 'm');
        let lim = '';
        if (s.limit_tokens || s.limit_requests) {
          const parts = [];
          if (s.limit_tokens) parts.push(fmtCompact(s.used_tokens) + '/' + fmtCompact(s.limit_tokens) + ' tok');
          if (s.limit_requests) parts.push(s.used_requests + '/' + s.limit_requests + ' req');
          lim = ' · ' + parts.join(', ') + (s.exhausted ? ' <b class="err">EXHAUSTED</b>' : '');
        }
        return '<b>' + s.provider + '</b> <span class="muted">[' + s.window + ']</span> ' + cnt + lim;
      });
      document.getElementById('quota').innerHTML =
        'quota: ' + (bits.length ? bits.join(' · ') : '<span class="muted">none configured</span>');
    }
  } catch (e) { /* quota strip optional */ }
}
setInterval(refresh, 3000); refresh();
setInterval(quotaRefresh, 5000); quotaRefresh();
</script>
</body>
</html>
`
