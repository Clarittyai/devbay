// The browser-facing half. It renders a page whose JavaScript calls the API
// directly, which is the case that forces the API address to be a *browser*
// address rather than a container one.
const http = require('http');
const API = process.env.API_URL || 'http://localhost:4000';
const BAY = process.env.DEVBAY_BAY || 'a bay';

http.createServer((req, res) => {
  res.setHeader('Content-Type', 'text/html; charset=utf-8');
  res.end(`<!doctype html><meta charset=utf-8><title>taskboard — ${BAY}</title>
<style>body{font:16px system-ui;margin:3rem auto;max-width:40rem}li{margin:.3rem 0}</style>
<h1>taskboard <small>${BAY}</small></h1>
<p>api: <code>${API}</code></p>
<form onsubmit="add(event)"><input id=t placeholder="a task"><button>add</button></form>
<ul id=list></ul>
<script>
const API = ${JSON.stringify(API)};
async function load() {
  const r = await fetch(API + '/tasks');
  const d = await r.json();
  list.innerHTML = d.tasks.map(t => '<li>' + t + '</li>').join('') || '<li><i>nothing yet</i></li>';
}
async function add(e) {
  e.preventDefault();
  await fetch(API + '/tasks?title=' + encodeURIComponent(t.value), {method: 'POST'});
  t.value = ''; load();
}
load();
</script>`);
}).listen(3000, '0.0.0.0', () => console.log('web listening on 3000'));
