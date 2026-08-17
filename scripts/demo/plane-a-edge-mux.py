#!/usr/bin/env python3
"""Tiny edge mux for the Plane-A Azure demo.

Production edge collectors usually terminate one HTTPS endpoint and route
OTLP + LLM proxy traffic internally. This script approximates that locally:

  /v1/traces  → Observer OTLP HTTP receiver (:4318)
  everything else → Observer LLM proxy (:8820)

Point cloudflared at this mux (default :8821) instead of :8820 directly so an
Azure chat app can use ONE public base URL for both OpenAI-compatible calls
and OTLP export — the same shape as a real deployment.
"""

from __future__ import annotations

import argparse
import http.client
import http.server
import socketserver
import sys
from typing import Tuple


def _forward(host: str, port: int, method: str, path: str, headers, body: bytes) -> Tuple[int, list, bytes]:
    conn = http.client.HTTPConnection(host, port, timeout=120)
    try:
        # Drop hop-by-hop + host so the backend sees a clean local request.
        drop = {"host", "content-length", "transfer-encoding", "connection", "keep-alive", "proxy-connection"}
        out_headers = {k: v for k, v in headers.items() if k.lower() not in drop}
        conn.request(method, path, body=body if body else None, headers=out_headers)
        resp = conn.getresponse()
        data = resp.read()
        resp_headers = [(k, v) for (k, v) in resp.getheaders() if k.lower() not in drop]
        return resp.status, resp_headers, data
    finally:
        conn.close()


class Handler(http.server.BaseHTTPRequestHandler):
    proxy_host = "127.0.0.1"
    proxy_port = 8820
    otlp_host = "127.0.0.1"
    otlp_port = 4318

    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def _handle(self) -> None:
        length = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(length) if length > 0 else b""
        path = self.path
        if path.startswith("/v1/traces") or path.startswith("/v1/logs") or path.startswith("/v1/metrics"):
            host, port = self.otlp_host, self.otlp_port
        else:
            host, port = self.proxy_host, self.proxy_port
        try:
            status, headers, data = _forward(host, port, self.command, path, self.headers, body)
        except Exception as exc:  # noqa: BLE001 — edge fail-open with 502
            msg = ("mux forward error: %s" % exc).encode()
            self.send_response(502)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        if data:
            self.wfile.write(data)

    def do_GET(self) -> None:  # noqa: N802
        self._handle()

    def do_POST(self) -> None:  # noqa: N802
        self._handle()

    def do_PUT(self) -> None:  # noqa: N802
        self._handle()

    def do_DELETE(self) -> None:  # noqa: N802
        self._handle()

    def do_OPTIONS(self) -> None:  # noqa: N802
        self._handle()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--listen", default="127.0.0.1:8821")
    ap.add_argument("--proxy", default="127.0.0.1:8820")
    ap.add_argument("--otlp", default="127.0.0.1:4318")
    args = ap.parse_args()
    listen_host, listen_port = args.listen.rsplit(":", 1)
    proxy_host, proxy_port = args.proxy.rsplit(":", 1)
    otlp_host, otlp_port = args.otlp.rsplit(":", 1)
    Handler.proxy_host, Handler.proxy_port = proxy_host, int(proxy_port)
    Handler.otlp_host, Handler.otlp_port = otlp_host, int(otlp_port)

    class ReuseTCPServer(socketserver.ThreadingTCPServer):
        allow_reuse_address = True

    with ReuseTCPServer((listen_host, int(listen_port)), Handler) as httpd:
        print(
            "plane-a edge mux on http://%s:%s  (proxy→%s  otlp→%s)"
            % (listen_host, listen_port, args.proxy, args.otlp),
            flush=True,
        )
        httpd.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
