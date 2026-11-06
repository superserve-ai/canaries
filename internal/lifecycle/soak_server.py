import http.server
import json
import os
import sys
import threading
import time
import urllib.parse
import uuid


# This identity exists only in this process's memory. A replacement process
# cannot recover it from the script or a file after a failed restore.
identity = {
    "instance_id": str(uuid.uuid4()),
    "pid": os.getpid(),
    "started_at": str(time.time_ns()),
}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        body = json.dumps({**identity, "challenge": query.get("challenge", [""])[0]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


servers = [http.server.ThreadingHTTPServer(("0.0.0.0", int(port)), Handler) for port in sys.argv[1:]]
for server in servers[:-1]:
    threading.Thread(target=server.serve_forever, daemon=True).start()
servers[-1].serve_forever()
