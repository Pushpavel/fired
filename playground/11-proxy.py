# A stand-in for your backend: owns the viewing domain, decides who sees
# what and who owns it, forwards to fired-artifacts with header routing.
import http.client, json, re, sys, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
API, TOKEN = "127.0.0.1:8444", "dev"
owners = {}  # artifact → (owner, fetched at)
def owner_of(aid):
    o = owners.get(aid)
    if not o or time.time() - o[1] > 5:
        req = urllib.request.Request(f"http://{API}/v1/artifacts/{aid}", headers={"X-Fired-Token": TOKEN})
        try: o = (json.load(urllib.request.urlopen(req)).get("labels", {}).get("owner"), time.time())
        except Exception: o = (None, time.time())
        owners[aid] = o
    return o[0]
STRIP = {"x-artifact", "x-artifact-version", "x-artifact-owner", "x-artifact-viewer", "x-fired-token", "authorization", "cookie", "host"}
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def handle_one(self):
        m = re.match(r"^([a-z0-9-]+?)(?:--v(\d+))?\.art\.localhost(:\d+)?$", self.headers.get("Host", ""))
        if not m: return self.plain(404, "not an artifact host")
        aid, ver = m.group(1), m.group(2)
        if self.path.startswith("/__login"):
            user = self.path.partition("user=")[2]
            self.send_response(302); self.send_header("Set-Cookie", f"user={user}; Path=/; HttpOnly; SameSite=Lax")
            self.send_header("Location", "/"); self.send_header("Content-Length", "0"); self.end_headers(); return
        user = None
        for c in (self.headers.get("Cookie") or "").split(";"):
            k, _, v = c.strip().partition("=")
            if k == "user": user = v
        hdrs = {k: v for k, v in self.headers.items() if k.lower() not in STRIP}
        hdrs.update({"X-Fired-Token": TOKEN, "X-Artifact": aid})
        if ver: hdrs["X-Artifact-Version"] = ver
        if user and user == owner_of(aid): hdrs["X-Artifact-Owner"] = "true"
        hdrs["X-Artifact-Viewer"] = user or self.client_address[0]  # for read limits
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0)) or None
        conn = http.client.HTTPConnection(API, timeout=300)
        conn.request(self.command, self.path, body=body, headers=hdrs)
        r = conn.getresponse()
        self.send_response(r.status)
        stream = (r.getheader("Content-Type") or "").startswith("text/event-stream")
        for k, v in r.getheaders():
            if k.lower() not in ("transfer-encoding", "connection", "content-length"): self.send_header(k, v)
        # The backend's own security headers for user-generated pages.
        self.send_header("Content-Security-Policy", "frame-ancestors 'self'")
        self.send_header("X-Viewer", user or "anonymous")
        if stream:
            self.send_header("Transfer-Encoding", "chunked"); self.end_headers()
            while True:
                chunk = r.read1(65536)
                if not chunk: break
                self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk)); self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
        else:
            data = r.read(); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def plain(self, code, msg):
        b = msg.encode(); self.send_response(code); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = handle_one
    def log_message(self, fmt, *a): sys.stderr.write("proxy: " + (fmt % a) + "\n")
ThreadingHTTPServer(("127.0.0.1", 8446), H).serve_forever()
