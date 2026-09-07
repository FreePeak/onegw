package server

// dashboardHTML is the minimal embedded dashboard: health, live usage table
// via /admin/usage polling. No external assets, no framework.
const dashboardHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>onegw</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; margin: 2rem; }
  h1 { font-size: 1.2rem; }
  table { border-collapse: collapse; margin-top: 1rem; min-width: 720px; }
  th, td { border: 1px solid #8884; padding: 4px 10px; text-align: right; }
  th:first-child, td:first-child { text-align: left; }
  .muted { opacity: .6; }
</style>
</head>
<body>
<h1>onegw <span class="muted">LLM gateway</span></h1>
<p><span id="health">checking…</span> · uptime <span id="uptime" class="muted">-</span></p>
<table id="usage">
  <thead><tr><th>provider</th><th>model</th><th>reqs</th><th>in tok</th><th>out tok</th><th>cache r</th><th>saved</th></tr></thead>
  <tbody></tbody>
</table>
<p class="muted">Endpoints: /v1/chat/completions · /v1/messages · /v1beta/models/{m}:generateContent · /v1/models</p>
<script>
async function refresh() {
  try {
    const h = await (await fetch('/admin/health')).json();
    document.getElementById('health').textContent = 'ok';
    const u = await (await fetch('/admin/usage')).json();
    const tb = document.querySelector('#usage tbody');
    tb.innerHTML = '';
    let rows = u.buckets || [];
    rows.sort((a,b) => (b.input+b.output)-(a.input+a.output));
    for (const b of rows) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td>'+b.provider+'</td><td>'+b.model+'</td><td>'+b.requests+
        '</td><td>'+b.input+'</td><td>'+b.output+'</td><td>'+b.cacheRead+'</td><td>'+b.saved+'</td>';
      tb.appendChild(tr);
    }
  } catch (e) { document.getElementById('health').textContent = 'error'; }
}
setInterval(refresh, 5000); refresh();
</script>
</body>
</html>
`
