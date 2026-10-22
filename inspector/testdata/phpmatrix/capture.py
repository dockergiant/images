"""Stand-in for the Inspector intake on :8126 that keeps every request on disk.

Each request becomes req-NNNN.json (method, path, headers) and req-NNNN.body
in the directory given as the first argument.
"""

import itertools
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

OUT = sys.argv[1]
COUNTER = itertools.count(1)
LOCK = threading.Lock()

# Same answer as the real intake in internal/apm/dd.go, so the tracer picks /v0.4/traces.
INFO = b'{"endpoints":["/v0.4/traces"],"client_drop_p0s":false}'


def read_body(handler):
    if handler.headers.get("Transfer-Encoding", "").lower() == "chunked":
        chunks = []
        while True:
            size = int(handler.rfile.readline().split(b";")[0].strip() or b"0", 16)
            if size == 0:
                while handler.rfile.readline() not in (b"\r\n", b"\n", b""):
                    pass
                return b"".join(chunks)
            chunks.append(handler.rfile.read(size))
            handler.rfile.readline()
    length = int(handler.headers.get("Content-Length") or 0)
    return handler.rfile.read(length) if length else b""


class Handler(BaseHTTPRequestHandler):
    # The sidecar keeps its connection open between flushes.
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("capture: " + (fmt % args) + "\n")

    def save(self, body):
        with LOCK:
            n = next(COUNTER)
        base = os.path.join(OUT, "req-%04d" % n)
        with open(base + ".body", "wb") as f:
            f.write(body)
        meta = {"method": self.command, "path": self.path, "headers": dict(self.headers.items())}
        with open(base + ".json", "w") as f:
            json.dump(meta, f)

    def reply(self, code, body=b"", ctype="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def handle_any(self):
        body = read_body(self) if self.command in ("POST", "PUT") else b""
        self.save(body)
        path = self.path.split("?", 1)[0]
        if path == "/info" and self.command == "GET":
            self.reply(200, INFO)
        elif path == "/v0.4/traces" and self.command in ("POST", "PUT"):
            self.reply(200, b'{"rate_by_service":{}}')
        elif path.startswith("/profiles/") and self.command in ("POST", "PUT"):
            self.reply(200)
        elif path.startswith("/telemetry/proxy/"):
            self.reply(202)
        else:
            self.reply(404, b"not found", "text/plain")

    do_GET = do_POST = do_PUT = handle_any


if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    ThreadingHTTPServer(("0.0.0.0", 8126), Handler).serve_forever()
