#!/usr/bin/env python3
"""Local preview server for website/ that mirrors Cloudflare Pages routing.

Cloudflare Pages serves `foo.html` at the clean URL `/foo` (and 308-redirects
`/foo.html` -> `/foo`). Python's stock `http.server` does neither, so links like
`/enterprise` or `/security` 404 locally even though they work in production.

This server resolves an extensionless request to its `.html` file when one
exists, so local review matches the live site.

Usage:
    python3 scripts/serve-website.py [port]      # default 8765
Then open http://localhost:8765/
"""
import os
import sys
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "website")
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8765


class PagesHandler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=ROOT, **kwargs)

    def translate_path(self, path):
        local = super().translate_path(path)
        # If the exact path is missing and isn't a directory, try `<path>.html`.
        if not os.path.exists(local) and not local.endswith(os.sep):
            if os.path.isfile(local + ".html"):
                return local + ".html"
        return local

    def end_headers(self):
        # Never cache during local review.
        self.send_header("Cache-Control", "no-store")
        super().end_headers()


if __name__ == "__main__":
    if not os.path.isdir(ROOT):
        sys.exit(f"website/ not found at {ROOT}")
    httpd = ThreadingHTTPServer(("127.0.0.1", PORT), PagesHandler)
    print(f"Serving {ROOT} at http://localhost:{PORT}/  (clean URLs: /enterprise, /security, ...)")
    print("Ctrl-C to stop.")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
