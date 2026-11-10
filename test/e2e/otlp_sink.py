#!/usr/bin/env python3
"""Minimal OTLP/HTTP (JSON) collector for the end-to-end test.

Usage: otlp_sink.py PORT OUT PARENT_PID

Appends the body of every POST /v1/traces as one line to OUT and exits
when PARENT_PID is gone. `check` mode summarises OUT:

  otlp_sink.py check OUT   -> prints "<trace id> <service>,<service>,..."
                              for every trace, most services first.
"""
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def check(path):
    services = {}
    names = {}
    with open(path) as f:
        for line in f:
            try:
                req = json.loads(line)
            except ValueError:
                continue
            for rs in req.get("resourceSpans", []):
                svc = "?"
                for kv in rs.get("resource", {}).get("attributes", []):
                    if kv.get("key") == "service.name":
                        svc = kv["value"].get("stringValue", "?")
                for ss in rs.get("scopeSpans", []):
                    for sp in ss.get("spans", []):
                        tid = sp.get("traceId", "")
                        services.setdefault(tid, set()).add(svc)
                        names.setdefault(tid, set()).add(sp.get("name", ""))
    for tid, svcs in sorted(services.items(), key=lambda kv: -len(kv[1])):
        print(tid, ",".join(sorted(svcs)), ",".join(sorted(names[tid])))


def serve(port, out, parent):
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", "0") or 0))
            if self.path == "/v1/traces":
                with lock, open(out, "ab") as f:
                    f.write(body.replace(b"\n", b" ") + b"\n")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b"{}")

        def log_message(self, *args):
            pass

    srv = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    while True:
        try:
            os.kill(parent, 0)
        except OSError:
            return
        time.sleep(1)


if __name__ == "__main__":
    if sys.argv[1] == "check":
        check(sys.argv[2])
    else:
        serve(int(sys.argv[1]), sys.argv[2], int(sys.argv[3]))
