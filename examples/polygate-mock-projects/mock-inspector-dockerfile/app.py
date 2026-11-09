"""Request inspector sample for the Dockerfile build method."""

import html
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = b'{"status":"ok"}'
            content_type = "application/json"
        elif self.path == "/api/request":
            body = json.dumps({"method": "GET", "path": self.path,
                               "userAgent": self.headers.get("User-Agent", "")[:120]}).encode()
            content_type = "application/json"
        elif self.path == "/":
            body = ("<!doctype html><html lang='en'><meta charset='utf-8'>"
                    "<meta name='viewport' content='width=device-width,initial-scale=1'>"
                    "<title>Request Inspector</title><style>body{font:18px system-ui;"
                    "max-width:580px;margin:8vh auto;padding:20px;color:#17254b}</style>"
                    "<h1>Request Inspector</h1><p>This Dockerfile-built sample is running.</p>"
                    "<p>Path: <code>" + html.escape(self.path) + "</code></p>"
                    "<p>Try <a href='/api/request'>/api/request</a> or <a href='/health'>/health</a>.</p></html>").encode()
            content_type = "text/html; charset=utf-8"
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
