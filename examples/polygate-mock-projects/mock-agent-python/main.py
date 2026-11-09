"""Tiny dependency-free mock agent for the PolyGate Python template."""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


PAGE = b"""<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Mock Agent</title><style>body{font:16px system-ui;max-width:620px;margin:8vh auto;padding:20px;color:#17254b}textarea,button{font:inherit;padding:12px}textarea{width:100%;box-sizing:border-box}button{background:#365bd6;color:white;border:0;border-radius:8px;margin-top:10px}pre{white-space:pre-wrap;background:#f2f5fb;padding:20px;border-radius:8px}</style><h1>Mock Agent</h1><p>Try a prompt. This sample returns a fixed local reply and does not call a model API.</p><textarea id="prompt" rows="4">Hello, PolyGate!</textarea><br><button onclick="send()">Ask agent</button><pre id="answer">Waiting for a prompt...</pre><script>async function send(){let r=await fetch('/api/chat',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({message:document.querySelector('#prompt').value})});document.querySelector('#answer').textContent=JSON.stringify(await r.json(),null,2)}</script></html>"""


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, body, content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/":
            return self.reply(200, PAGE, "text/html; charset=utf-8")
        if self.path == "/health":
            return self.reply(200, b'{"status":"ok"}')
        self.reply(404, b'{"error":"not_found"}')

    def do_POST(self):
        if self.path != "/api/chat":
            return self.reply(404, b'{"error":"not_found"}')
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length > 4096:
                return self.reply(413, b'{"error":"message_too_large"}')
            message = json.loads(self.rfile.read(length)).get("message", "")
            if not isinstance(message, str):
                raise ValueError("message must be a string")
            body = json.dumps({"reply": f"Mock response: I received {message[:200]!r}.",
                               "model": "local-demo", "tokensUsed": 0}).encode()
            self.reply(200, body)
        except (ValueError, json.JSONDecodeError):
            self.reply(400, b'{"error":"invalid_request"}')


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
