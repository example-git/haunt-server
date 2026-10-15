#!/usr/bin/env python3
"""Test the complete bundle in a new disposable local container and volume.

Never reads deployment environment files, mounts existing data or connects to a
host/remote database. Cleanup names are generated and confined to this test run.
"""
import base64
import json
import os
import re
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid
import zlib

DOCKER = ["docker", "--host=unix:///var/run/docker.sock"]


def run(*args, check=True, input=None):
    result = subprocess.run([str(x) for x in args], input=input, capture_output=True, text=True)
    if check and result.returncode:
        # Commands can contain test credentials; don't dump their argv.
        raise RuntimeError(result.stderr.strip() or result.stdout.strip() or "Command failed")
    return result


def docker(*args, **kwargs):
    return run(*DOCKER, *args, **kwargs)


def inspect(name):
    return json.loads(docker("inspect", name).stdout)[0]


def websocket(ip, port, token):
    with socket.create_connection((ip, port), timeout=5) as sock:
        key = base64.b64encode(os.urandom(16)).decode()
        sock.sendall(("GET /api/v1/events HTTP/1.1\r\nHost: localhost\r\nOrigin: http://localhost\r\n"
                      "Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
                      f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
        stream = sock.makefile("rb")
        assert b" 101 " in stream.readline()
        while stream.readline() != b"\r\n":
            pass
        payload = json.dumps({"type": "authenticate", "token": token}).encode()
        mask = os.urandom(4)
        length = bytes([len(payload) | 128]) if len(payload) < 126 else bytes([254]) + struct.pack("!H", len(payload))
        sock.sendall(b"\x81" + length + mask + bytes(c ^ mask[i % 4] for i, c in enumerate(payload)))
        first, length = stream.read(2)
        assert first & 15 == 1 and length & 128 == 0
        if length == 126:
            length = struct.unpack("!H", stream.read(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", stream.read(8))[0]
        assert json.loads(stream.read(length))["type"] == "ready"


def main():
    image = sys.argv[1] if len(sys.argv) > 1 else "haunt:container-check"
    ip, port = "127.0.0.1", 0
    suffix = uuid.uuid4().hex[:12]
    name, volume = "haunt-container-" + suffix, "haunt-container-" + suffix
    volumes = [volume]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(path, method="GET", value=None, token=None, expected=200, headers=None):
        data = value if isinstance(value, bytes) else json.dumps(value).encode() if value is not None else None
        hdr = {"Host": "localhost", **(headers or {})}
        if data and not isinstance(value, bytes):
            hdr["Content-Type"] = "application/json"
        if token:
            hdr["Authorization"] = "Bearer " + token
        req = urllib.request.Request(f"http://{ip}:{port}" + path, data=data, headers=hdr, method=method)
        try:
            response = opener.open(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            body = response.read()
            assert response.status == expected, (method, path, response.status, expected)
            return body, response.headers

    def api(*args, **kwargs):
        return json.loads(request(*args, **kwargs)[0])

    def sql(statement):
        return docker("exec", name, "su-exec", "postgres", "psql", "-h", "/run/postgresql", "-XAt", "-d", "haunt", "-c", statement).stdout.strip()

    def start(tls=False, extra=()):
        nonlocal port
        docker("run", "-d", "--name", name, "-p", "127.0.0.1::80",
               "--mount", "source=" + volume + ",target=/data", "--stop-timeout", "65",
               "-e", "HAUNT_PUBLIC_URL=" + ("https://localhost" if tls else "http://localhost"),
               "-e", "HAUNT_TLS_MODE=" + ("provided" if tls else "proxy"),
               *extra, image)
        bindings = inspect(name)["NetworkSettings"]["Ports"].get("80/tcp")
        port = int(bindings[0]["HostPort"]) if bindings else 0

    def wait_ready():
        for _ in range(80):
            state = inspect(name)["State"]
            if not state["Running"]:
                raise RuntimeError(docker("logs", "--tail", "30", name).stdout + docker("logs", "--tail", "30", name).stderr)
            if docker("exec", name, "/opt/haunt/healthcheck.sh", check=False).returncode == 0:
                return
            time.sleep(0.5)
        raise RuntimeError("Container readiness timed out")

    def stop():
        docker("stop", "-t", "65", name)
        assert inspect(name)["State"]["ExitCode"] == 0, "Container did not stop cleanly"
        docker("rm", name)

    docker("volume", "create", volume)
    try:
        start()
        wait_ready()
        password = docker("exec", name, "cat", "/data/config/owner-password").stdout.strip()
        instance = api("/api/v1/instance")
        assert instance["public_url"] == "http://localhost"
        html, headers = request("/")
        assert b"Haunt" in html and headers["Cache-Control"] == "no-store"
        assert "wasm-unsafe-eval" in headers["Content-Security-Policy"]
        assert request("/haunt-build.json")[1]["Cache-Control"] == "no-store"
        asset = re.search(rb'src="(/assets/[^\"]+\.js)"', html).group(1).decode()
        assert "immutable" in request(asset)[1]["Cache-Control"]
        assert request("/invite/example")[0] == html
        for path in ["/api/not-real", "/client-updates/not-real", "/assets/missing.js", "/.env"]:
            request(path, expected=404)
        assert "openapi" in api("/openapi.json")
        request("/api/v1/instance", expected=403, headers={"Origin": "https://untrusted.invalid"})
        token = api("/api/v1/auth/login", "POST", {"username": "owner", "password": password, "device_name": "container check", "remember": True})["token"]
        # Direct callers cannot forge the IP used for bans and audit records.
        api("/api/v1/auth/login", "POST", {"username": "owner", "password": password, "device_name": "spoof check"},
            headers={"X-Forwarded-For": "203.0.113.77", "CF-Connecting-IP": "203.0.113.77"})
        assert sql("SELECT count(*) FROM chatxo.sessions WHERE remote_ip='203.0.113.77'") == "0"
        channel = api("/api/v1/channels", "POST", {"name": "container-check", "kind": "text"}, token=token, expected=201)
        messages = "/api/v1/conversations/" + channel["id"] + "/messages"
        api(messages, "POST", {"request_id": str(uuid.uuid4()), "body": "survives replacement"}, token=token, expected=201)
        def chunk(kind, content):
            return struct.pack("!I", len(content)) + kind + content + struct.pack("!I", zlib.crc32(kind + content))
        png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack("!2I5B", 2, 2, 8, 2, 0, 0, 0))
        png += chunk(b"IDAT", zlib.compress((b"\x00" + b"\xff\x00\x00" * 2) * 2)) + chunk(b"IEND", b"")
        api("/api/v1/conversations/" + channel["id"] + "/attachments?filename=check.png&size=" + str(len(png)),
            "POST", png, token=token, expected=201)
        websocket(ip, port, token)
        docker("exec", name, "ffprobe", "-version")
        # Each component runs under its own identity. PostgreSQL has no TCP listener.
        procs = docker("exec", name, "ps", "-o", "user,args").stdout
        assert re.search(r"haunt\s+/data/apps/current/haunt", procs)
        assert re.search(r"postgres\s+.*postgres -D /data/postgres", procs)
        roles = sql("SELECT rolname,rolsuper,rolcreatedb,rolcreaterole,rolbypassrls FROM pg_roles WHERE rolname='haunt'")
        assert roles == "haunt|f|f|f|f"
        assert docker("exec", name, "su-exec", "postgres", "psql", "-h", "/run/postgresql", "-XAt", "-d", "haunt", "-c", "SHOW listen_addresses").stdout.strip() == ""
        assert docker("exec", name, "psql", "-h", "/run/postgresql", "-U", "postgres", "-d", "haunt", "-c", "SELECT 1", check=False).returncode != 0
        assert password not in docker("logs", name).stdout + docker("logs", name).stderr
        print("PASS: static client/cache headers, API/docs/login, messaging/uploads, IP spoof rejection, CORS, authenticated WebSocket, ffprobe and restricted processes/database role", flush=True)
        stop()
        # Follow the documented cold-backup recipe and restore into a different
        # empty volume before checking identity, owner, session and messages.
        with tempfile.TemporaryFile() as backup:
            subprocess.run([*DOCKER, "run", "--rm", "--mount", "source=" + volume + ",target=/data",
                            image, "tar", "-C", "/data", "-czf", "-", "."], stdout=backup, check=True)
            volume += "-restored"
            volumes.append(volume)
            docker("volume", "create", volume)
            backup.seek(0)
            subprocess.run([*DOCKER, "run", "--rm", "-i", "--mount", "source=" + volume + ",target=/data",
                            image, "tar", "-C", "/data", "-xzf", "-"], stdin=backup, check=True)
        start()
        wait_ready()
        assert api("/api/v1/instance")["instance_id"] == instance["instance_id"]
        assert api(messages, token=token)["messages"][0]["body"] == "survives replacement"
        assert docker("exec", name, "cat", "/data/config/owner-password").stdout.strip() == password
        print("Image bytes:", inspect(image)["Size"], flush=True)
        print("Idle resource sample:", docker("stats", "--no-stream", "--format", "{{.MemUsage}}; CPU {{.CPUPerc}}", name).stdout.strip(), flush=True)
        print("PASS: clean shutdown, cold backup/restore, saved installation/owner/session/message persistence", flush=True)
        old_pid = docker("exec", name, "supervisorctl", "pid", "nginx").stdout.strip()
        docker("exec", name, "sh", "-c", "kill -TERM $(cat /run/haunt/nginx.pid)")
        for _ in range(30):
            new_pid = docker("exec", name, "supervisorctl", "pid", "nginx").stdout.strip()
            if new_pid != old_pid and new_pid != "0":
                break
            time.sleep(0.5)
        assert new_pid != old_pid and new_pid != "0"
        wait_ready()
        docker("exec", name, "supervisorctl", "restart", "postgres")
        wait_ready()
        assert api(messages, token=token)["messages"][0]["body"] == "survives replacement"
        print("PASS: Supervisor recovers Nginx and restarts PostgreSQL without losing messages", flush=True)
        stop()
        # Make a short-lived local cert; never contact ACME for automated tests.
        docker("run", "--rm", "--mount", "source=" + volume + ",target=/data", image, "sh", "-c",
               "mkdir -p /data/tls && openssl req -x509 -newkey rsa:2048 -nodes -keyout /data/tls/privkey.pem -out /data/tls/fullchain.pem -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost")
        start(tls=True)
        wait_ready()
        docker("exec", name, "curl", "--fail", "--silent", "--cacert", "/data/tls/fullchain.pem", "https://localhost/api/v1/instance")
        redirect = docker("exec", name, "curl", "-sSI", "http://localhost/test?x=1").stdout
        assert "308" in redirect and "https://localhost/test?x=1" in redirect
        print("PASS: supplied HTTPS certificate and HTTP-to-HTTPS redirect", flush=True)
        # Publish test releases through this container's own trusted local TLS
        # endpoint. Exercise the real downloader, checksum and recovery paths.
        from pathlib import Path
        fixture = Path(__file__).with_name("container-update-fixture.py").read_text()
        def offer(broken=False):
            docker("exec", "-i", name, "python3", "-", "broken" if broken else "valid", input=fixture)
        def update(check=True):
            return docker("exec", "-e", "SSL_CERT_FILE=/data/tls/fullchain.pem", "-e",
                          "HAUNT_APP_UPDATE_MANIFEST_URL=https://localhost/test-updates/haunt-app.json",
                          name, "haunt-update", "apply", check=check)
        offer()
        manifest_path = "/data/apps/current/web/test-updates/haunt-app.json"
        docker("exec", name, "cp", manifest_path, manifest_path + ".valid")
        docker("exec", name, "python3", "-c", "import json,pathlib; p=pathlib.Path('" + manifest_path + "'); d=json.loads(p.read_text()); [a.update(sha256='0'*64) for a in d['platforms'].values()]; p.write_text(json.dumps(d))")
        assert update(check=False).returncode != 0
        wait_ready()
        docker("exec", name, "mv", manifest_path + ".valid", manifest_path)
        before = docker("exec", name, "readlink", "/data/apps/current").stdout
        update()
        after = docker("exec", name, "readlink", "/data/apps/current").stdout
        assert before != after
        assert sql("SELECT body FROM chatxo.messages WHERE body='survives replacement'") == "survives replacement"
        offer(broken=True)
        assert update(check=False).returncode != 0
        wait_ready()
        assert docker("exec", name, "readlink", "/data/apps/current").stdout == after
        assert sql("SELECT body FROM chatxo.messages WHERE body='survives replacement'") == "survives replacement"
        stop()
        start(tls=True)
        wait_ready()
        assert docker("exec", name, "readlink", "/data/apps/current").stdout == after
        print("PASS: wrong-hash rejection, application update, failed-release database/files rollback and update persistence across restart", flush=True)
        stop()
        start(extra=("-e", "HAUNT_INSTANCE_ID=" + str(uuid.uuid4())))
        assert docker("wait", name).stdout.strip() == "1"
        assert "differs from the saved installation" in docker("logs", name).stderr
        print("PASS: mismatched installation identity rejected", flush=True)
    finally:
        docker("rm", "-f", name, check=False)
        for saved in volumes:
            docker("volume", "rm", saved, check=False)


if __name__ == "__main__":
    main()
