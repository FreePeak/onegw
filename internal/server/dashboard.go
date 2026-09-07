package server

// dashboardHTML is the minimal embedded dashboard: health + memory stats,
// cumulative totals, and the live per-key window table via /admin/usage
// polling. The admin password is stored in localStorage and attached to
// admin API calls (the health endpoint is open). No external assets.
const dashboardHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>onegw</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; margin: 2rem; }
  h1 { font-size: 1.2rem; }
  table { border-collapse: collapse; margin-top: 1rem; min-width: 760px; }
  th, td { border: 1px solid #8884; padding: 4px 10px; text-align: right; }
  th:first-child, td:first-child { text-align: left; }
  .muted { opacity: .6; }
  .err { color: #c0392b; }
  input { font: inherit; padding: 2px 6px; }
</style>
</head>
<body>
<h1>onegw <span class="muted">LLM gateway</span></h1>
<p>
  <span id="health">checking…</span> · uptime <span id="uptime" class="muted">-</span>
  · heap <span id="heap" class="muted">-</span> · sys <span id="sys" class="muted">-</span>
</p>
<p id="totals">totals: <span class="muted">-</span></p>
<p class="muted">window table below resets each flush interval; totals above count since process start</p>
<p>
  admin password: <input id="pw" type="password" placeholder="(admin_password from config)" size="28">
  <button onclick="savePw()">save</button> <span id="authstate" class="muted"></span>
</p>
<table id="usage">
  <thead><tr><th>provider</th><th>model</th><th>reqs</th><th>in tok</th><th>out tok</th><th>cache r</th><th>saved</th></tr></thead>
  <tbody></tbody>
</table>
<p class="muted">Endpoints: /v1/chat/completions · /v1/messages · /v1beta/models/{m}:generateContent · /v1/models</p>
<script>
const pwInput = document.getElementById('pw');
pwInput.value = localStorage.getItem('onegw_admin') || '';
function savePw() { localStorage.setItem('onegw_admin', pwInput.value); refresh(); }
function authed(url) {
  const pw = localStorage.getItem('onegw_admin') || '';
  if (!pw) return url;
  return url + (url.includes('?') ? '&' : '?') + 'password=' + encodeURIComponent(pw);
}
async function refresh() {
  try {
    const h = await (await fetch('/admin/health')).json();
    document.getElementById('health').textContent = 'ok';
    document.getElementById('health').className = '';
    document.getElementById('uptime').textContent = h.uptime_s + 's';
    document.getElementById('heap').textContent = h.heap_alloc_mb + ' MiB';
    document.getElementById('sys').textContent = h.sys_mb + ' MiB';
  } catch (e) {
    document.getElementById('health').textContent = 'error';
    document.getElementById('health').className = 'err';
  }
  try {
    const resp = await fetch(authed('/admin/usage?source=store&days=1'));
    if (resp.status === 401) {
      document.getElementById('authstate').textContent = 'unauthorized — enter password';
      return;
    }
    document.getElementById('authstate').textContent = '';
    const u = await resp.json();
    // Table shows persisted store rollups (days=1) so values do not reset
    // under the refresh interval; totals line keeps the live window.
    const tb = document.querySelector('#usage tbody');
    tb.innerHTML = '';
    const rows = u.rows || [];
    rows.sort((a, b) => (b.input + b.output) - (a.input + a.output));
    for (const b of rows) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td>' + b.provider + '</td><td>' + b.model + '</td><td>' + b.requests +
        '</td><td>' + b.input + '</td><td>' + b.output + '</td><td>' + b.cacheRead + '</td><td>' + b.saved + '</td>';
      tb.appendChild(tr);
    }
  } catch (e) { /* health still shown */ }
}
setInterval(refresh, 3000); refresh();
</script>
</body>
</html>
`
