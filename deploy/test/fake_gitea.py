"""A fake Gitea API for testing deploy/setup-gitea-mirrors.sh (deploy/test/setup-gitea-mirrors.test.sh).

Serves HTTPS with the given certificate, accepts only "Authorization: token <expected>", and records every
call as JSON lines in the log file so the test can check what the script did.

    python3 -I fake_gitea.py <port> <cert.pem> <key.pem> <expected-token> <log-file>
"""

import json
import re
import ssl
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, CERT, KEY, TOKEN, LOG = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
repos = {"tester/ludworks-browser": {"mirror": True}}  # one mirror exists already


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status, body=None):
        # Compact, like Gitea (Go's encoding/json).
        data = json.dumps(body, separators=(",", ":")).encode() if body is not None else b""
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def handle_any(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        body = json.loads(raw) if raw else None
        with open(LOG, "a") as f:
            f.write(json.dumps({"method": self.command, "path": self.path, "body": body,
                                "auth": self.headers.get("Authorization")}) + "\n")
        if self.headers.get("Authorization") != "token " + TOKEN:
            return self.reply(401, {"message": "token is required"})
        p = self.path
        if self.command == "GET" and p == "/api/v1/user":
            return self.reply(200, {"id": 1, "login": "tester"})
        m = re.fullmatch(r"/api/v1/repos/([^/]+/[^/]+)", p)
        if m and self.command == "GET":
            r = repos.get(m.group(1))
            return self.reply(200, {"full_name": m.group(1), **r}) if r else self.reply(404, {"message": "not found"})
        if m and self.command == "PATCH":
            return self.reply(200, {"full_name": m.group(1)}) if m.group(1) in repos else self.reply(404, {})
        if self.command == "POST" and p == "/api/v1/repos/migrate":
            repos[body["repo_owner"] + "/" + body["repo_name"]] = {"mirror": body.get("mirror", False)}
            return self.reply(201, {"full_name": body["repo_owner"] + "/" + body["repo_name"]})
        if re.fullmatch(r"/api/v1/repos/[^/]+/[^/]+/actions/secrets/[A-Z_]+", p) and self.command == "PUT":
            return self.reply(201)
        if re.fullmatch(r"/api/v1/repos/[^/]+/[^/]+/actions/variables/[A-Z_]+", p) and self.command == "POST":
            return self.reply(201)
        return self.reply(404, {"message": "unexpected " + self.command + " " + p})

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = handle_any


class Server(ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        pass  # aborted handshakes (the pinning test) are expected


server = Server(("127.0.0.1", PORT), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(CERT, KEY)
server.socket = ctx.wrap_socket(server.socket, server_side=True)
server.serve_forever()
