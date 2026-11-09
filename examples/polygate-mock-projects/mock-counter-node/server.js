const http = require('node:http')

let count = 0
const page = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Mock Counter</title><style>body{font:18px system-ui;max-width:580px;margin:8vh auto;padding:20px;color:#17254b}button{background:#365bd6;color:white;border:0;border-radius:8px;padding:12px 18px;font:inherit}</style><h1>Mock Counter</h1><p>Requests served by this container: <strong id="count">0</strong></p><button onclick="refresh()">Refresh count</button><p>The count is stored in memory and resets after a restart.</p><script>async function refresh(){let r=await fetch('/api/count');document.querySelector('#count').textContent=(await r.json()).count}refresh()</script></html>`

http.createServer((request, response) => {
  const send = (status, type, body) => { response.writeHead(status, { 'content-type': type }); response.end(body) }
  if (request.url === '/health') return send(200, 'application/json', JSON.stringify({ status: 'ok' }))
  if (request.url === '/api/count') return send(200, 'application/json', JSON.stringify({ count: ++count }))
  if (request.url === '/') return send(200, 'text/html; charset=utf-8', page)
  send(404, 'application/json', JSON.stringify({ error: 'not_found' }))
}).listen(3000, '0.0.0.0')
