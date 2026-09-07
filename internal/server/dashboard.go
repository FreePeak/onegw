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
  input { font: inherit; padding: 2px 6px; }
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
<p id="totals">usage (since process start): <span class="muted">enter admin password to view</span></p>
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
function authed(url) {
  const pw = localStorage.getItem('onegw_admin') || '';
  if (!pw) return url;
  return url + (url.includes('?') ? '&' : '?') + 'password=' + encodeURIComponent(pw);
}
function fmtK(n) { return n >= 1000000 ? (n/1000000).toFixed(1) + 'M' : n >= 1000 ? (n/1000).toFixed(1) + 'K' : n; }
async function refresh() {
  try {
    const hr = await fetch(authed('/admin/health'));
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
    // days=2 is a coarse UTC fetch wide enough for any timezone; the exact
    // browser-local day filter happens below (store rows carry UTC day+hour).
    const resp = await fetch(authed('/admin/usage?source=store&days=2'));
    if (resp.status === 401) {
      document.getElementById('authstate').textContent = 'unauthorized — enter password';
      return;
    }
    document.getElementById('authstate').textContent = '';
    const u = await resp.json();
    // Keep only rows inside the viewer's local calendar day so the numbers
    // match the user's timezone, not UTC.
    const startMs = new Date(); startMs.setHours(0, 0, 0, 0);
    const win = (u.rows || []).filter(r => Date.UTC(
      +r.day.slice(0, 4), +r.day.slice(5, 7) - 1, +r.day.slice(8, 10), +(r.hour || 0)) >= startMs.getTime());
    // Totals and the table are computed from the same filtered rows, so the
    // header can never disagree with the table.
    const t = { requests: 0, input: 0, output: 0, saved: 0 };
    for (const r of win) {
      t.requests += r.requests || 0; t.input += r.input || 0;
      t.output += r.output || 0; t.saved += r.saved || 0;
    }
    document.getElementById('totals').innerHTML =
      '<b>' + t.requests + '</b> reqs · in <b>' + fmtK(t.input) +
      '</b> tok · out <b>' + fmtK(t.output) + '</b> tok · saved <b>' + fmtK(t.saved) +
      '</b> tok <span class="muted">(today, your local time; table = same window per provider+model)</span>';
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
        '</td><td>' + fmtK(b.input) + '</td><td>' + fmtK(b.output) + '</td><td>' + fmtK(b.cacheRead) + '</td><td>' + fmtK(b.saved) + '</td>';
      tb.appendChild(tr);
    }
  } catch (e) { /* health still shown */ }
}
setInterval(refresh, 3000); refresh();
</script>
</body>
</html>
`
