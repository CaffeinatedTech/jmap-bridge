#!/usr/bin/env python3
"""M10 live gate — Gmail API mode, read-only, against the user's real
Gmail account (approved 2026-10-03). It never touches existing mail: it
creates ONE test label and ONE test message inside it, proves the bridge
browses them through the Gmail API backend, observes a foreign flag change
made out-of-band through the Gmail API itself, then deletes exactly the
test message and label it created.

Secrets stay in .env (0600) and are never printed. Backfill is scoped to
the test label so the gate caches a handful of messages, not the mailbox.
"""
import base64
import json
import os
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

REPO = "/home/adam/projects/jmap-bridge"
OLD_DATA = os.path.join(REPO, "data")
DATA = "/tmp/opencode/gmailapi-live-data"
KEYFILE = "/tmp/opencode/gmail-key"
LABEL = "jmap-bridge-test"
# 8080 matches the OAuth client's registered redirect URI
# (http://127.0.0.1:8080/oauth/gmail/callback).
BRIDGE = "http://127.0.0.1:8080/gmail"
TOKEN = "dev-token-gmail-local-only-0123456789"
AUTH = "Basic " + base64.b64encode(("any:" + TOKEN).encode()).decode()
SUBJECT = "jmap-bridge M10 API gate"


def load_env():
    for line in open(os.path.join(REPO, ".env")):
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"'))
    key = os.environ.get("JMAP_BRIDGE_SECRET_KEY")
    if key and not os.path.exists(KEYFILE):
        fd = os.open(KEYFILE, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        os.write(fd, key.encode())
        os.close(fd)
    client_id = None
    for line in open(os.path.join(REPO, "dev/config-gmail.toml")):
        if "client_id" in line:
            client_id = line.split("=", 1)[1].strip().strip('"')
            break
    if client_id:
        os.environ.setdefault("GMAIL_TOKEN_CLIENT_ID", client_id)


def build():
    subprocess.run(["go", "build", "-o", "/tmp/opencode/gmailtoken", "./test/live/gmailtoken"],
                   cwd=REPO, check=True)
    subprocess.run(["go", "build", "-o", "/tmp/opencode/jmap-bridge-gapi", "./cmd/jmap-bridge"],
                   cwd=REPO, check=True)


def access_token(data_dir=DATA):
    out = "/tmp/opencode/gmail.access"
    subprocess.run(["/tmp/opencode/gmailtoken", data_dir, KEYFILE, out],
                   cwd=REPO, check=True,
                   env={**os.environ, "GMAIL_TOKEN_ACCOUNT": "gmail"})
    return open(out).read().strip()


def has_stored_token(db_dir):
    try:
        con = sqlite3.connect(db_dir + "/bridge.db")
        try:
            return con.execute("select count(*) from oauth_tokens where account='gmail'").fetchone()[0] > 0
        finally:
            con.close()
    except Exception:
        return False


def api(method, path, body=None, token=None):
    url = "https://gmail.googleapis.com/gmail/v1/users/me" + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", "Bearer " + (token or ""))
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        if method == "DELETE" and e.code == 404:
            return {}
        raise


def jmap(calls, token=TOKEN, base=BRIDGE):
    req = urllib.request.Request(base + "/jmap",
                                 data=json.dumps({"using": ["urn:ietf:params:jmap:core",
                                                            "urn:ietf:params:jmap:mail"],
                                                  "methodCalls": calls}).encode(),
                                 method="POST")
    req.add_header("Authorization", AUTH)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        raise RuntimeError(f"JMAP HTTP {e.code}: {body[:500]}") from None


def write_bridge_config():
    os.makedirs(DATA, exist_ok=True)
    cfg = f'''listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "{DATA}"
log_level = "info"

[sync]
interval = "5s"

[auth]
mode = "token"

[[accounts]]
id = "gmail"
name = "Gmail API"
address = "you@gmail.com"
token = "{TOKEN}"
backend = "gmail_api"

  [accounts.oauth2]
  provider = "google"
  client_id = "{os.environ["GMAIL_TOKEN_CLIENT_ID"]}"

  [accounts.gmail_api]
  backfill_query = "label:{LABEL}"
  quota_units_per_second = 100
'''
    with open("/tmp/opencode/gmailapi-live.toml", "w") as f:
        f.write(cfg)


def copy_token():
    src = sqlite3.connect(OLD_DATA + "/bridge.db")
    dst = sqlite3.connect(DATA + "/bridge.db")
    try:
        row = src.execute(
            "select account, refresh_token, access_token, access_expiry, updated_at "
            "from oauth_tokens where account='gmail'").fetchone()
        if not row:
            sys.exit("no stored Gmail refresh token in the M4 data dir")
        dst.execute("insert or replace into oauth_tokens values (?,?,?,?,?)", row)
        dst.commit()
    finally:
        src.close()
        dst.close()


def start_bridge():
    log = open("/tmp/opencode/bridge-gmailapi-live.log", "a")
    p = subprocess.Popen(["/tmp/opencode/jmap-bridge-gapi", "--config",
                          "/tmp/opencode/gmailapi-live.toml"],
                         stdout=log, stderr=subprocess.STDOUT, cwd=REPO,
                         env={**os.environ, "JMAP_BRIDGE_SECRET_KEY": os.environ["JMAP_BRIDGE_SECRET_KEY"]})
    return p


def wait(pred, timeout, what):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return True
        time.sleep(0.5)
    return False


def session_ok():
    try:
        req = urllib.request.Request(BRIDGE + "/.well-known/jmap")
        req.add_header("Authorization", AUTH)
        with urllib.request.urlopen(req, timeout=5) as r:
            d = json.loads(r.read())
        return "urn:ietf:params:jmap:mail" in d.get("capabilities", {})
    except Exception:
        return False


def main():
    load_env()
    build()
    write_bridge_config()

    # Migrate the fresh data dir, then obtain a usable refresh token: the
    # fresh dir's own (from a previous consent) wins; otherwise adopt the
    # M4 token if it still refreshes; otherwise the operator must consent.
    p = start_bridge()
    time.sleep(1.0)
    p.terminate()
    p.wait()

    if not has_stored_token(DATA):
        try:
            copy_token()
        except SystemExit:
            pass
    try:
        access_token()
    except subprocess.CalledProcessError:
        print("No usable Gmail refresh token. Approve consent once, then re-run:")
        print(f"  open http://127.0.0.1:8080/oauth/gmail/start")
        sys.exit(3)

    p = start_bridge()
    token = None
    label_id = None
    msg_id = None
    try:
        if not wait(session_ok, 30, "bridge session"):
            sys.exit("FAIL: bridge never served a session")
        print("PASS session served over API mode")

        token = access_token()

        # Create the test label (reuse if it already exists).
        labels = api("GET", "/labels", token=token).get("labels", [])
        existing = next((l for l in labels if l["name"] == LABEL), None)
        label_id = existing["id"] if existing else api("POST", "/labels", {"name": LABEL}, token=token)["id"]
        print(f"PASS test label {LABEL}:{label_id}")

        # Insert one test message into the label (never touches existing mail).
        raw = (f"From: you@gmail.com\r\nTo: you@gmail.com\r\n"
               f"Subject: {SUBJECT}\r\nMessage-ID: <m10-gate-{int(time.time())}@jmap-bridge.test>\r\n"
               f"Date: Mon, 02 Jan 2026 15:04:05 -0700\r\nMIME-Version: 1.0\r\n"
               f"Content-Type: text/plain; charset=utf-8\r\n\r\nM10 API gate body\r\n").encode()
        msg = api("POST", "/messages?internalDateSource=dateHeader",
                  {"raw": base64.urlsafe_b64encode(raw).rstrip(b"=").decode(),
                   "labelIds": [label_id]}, token=token)
        msg_id = msg["id"]
        print(f"PASS test message inserted: {msg_id}")

        if not wait(session_ok, 5, "session"):
            sys.exit("FAIL: session lost")

        # The bridge must discover roles and the test message.
        def mailboxes():
            r = jmap([["Mailbox/get", {"accountId": "gmail"}, "m"]])["methodResponses"][0][1]["list"]
            return {m["role"]: m["id"] for m in r if m.get("role")}

        roles = {}
        if wait(lambda: (roles.update(mailboxes()) or True) and {"inbox", "sent", "archive"} <= set(roles),
                30, "roles"):
            print("PASS mailboxes expose inbox/sent/archive roles")
        else:
            sys.exit(f"FAIL: roles missing: {sorted(roles)}")

        def find_msg():
            q = jmap([["Email/query", {"accountId": "gmail"}, "q"]])
            ids = q["methodResponses"][0][1]["ids"]
            if not ids:
                return None
            g = jmap([["Email/get", {"accountId": "gmail", "ids": ids,
                                     "properties": ["id", "subject", "keywords"]}, "g"]])
            for e in g["methodResponses"][0][1]["list"]:
                if e.get("subject") == SUBJECT:
                    return e
            return None

        e = None
        deadline = time.time() + 60
        while time.time() < deadline:
            e = find_msg()
            if e:
                break
            time.sleep(1)
        if not e:
            sys.exit("FAIL: test message never appeared in the bridge")
        print(f"PASS Email/query + Email/get returned {SUBJECT!r}")

        # Foreign change out-of-band: star it, the bridge must reflect it.
        api("POST", f"/messages/{msg_id}/modify", {"addLabelIds": ["STARRED"]}, token=token)
        got = wait(lambda: (find_msg() or {}).get("keywords", {}).get("$flagged", False),
                   60, "flagged")
        if not got:
            sys.exit("FAIL: foreign flag change not observed")
        print("PASS foreign STARRED change observed through history incremental")
    finally:
        p.terminate()
        try:
            p.wait(timeout=10)
        except Exception:
            p.kill()
        # Cleanup: remove exactly what this gate created.
        try:
            if msg_id:
                api("DELETE", f"/messages/{msg_id}?permanent=true", token=token)
            if label_id:
                api("DELETE", f"/labels/{label_id}", token=token)
            print("PASS cleaned up the test message and label")
        except Exception as ex:  # noqa: BLE001
            print(f"WARN cleanup: {ex}")


if __name__ == "__main__":
    main()
