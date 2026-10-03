#!/usr/bin/env python3
"""M11 live write gate — Gmail API mode writes against a Gmail account.

Intended for a **dedicated throwaway** account, but safe on any account: it
only ever touches objects whose names/message-id it mints (the
``jmap-bridge-test`` prefix), and it aborts on the first throttle. It
exercises the whole M11 surface through the bridge's JMAP API and re-reads
every claim independently through the Gmail API itself:

  label both ways, archive, star, move, Mailbox/set (create/rename/delete +
  refusing onDestroyRemoveEmails=true), draft create, destroy.

Secrets stay in .env (0600) and are never printed. Backfill is scoped to the
test label so the gate caches a handful of messages, not the mailbox.
"""
import base64
import json
import os
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request

REPO = "/home/adam/projects/jmap-bridge"
OLD_DATA = os.path.join(REPO, "data")
DATA = "/tmp/opencode/gmailapi-live-data"
KEYFILE = "/tmp/opencode/gmail-key"
LABEL = "jmap-bridge-test"
MOVE_LABEL = "jmap-bridge-test-move"
MAILBOX = "jmap-bridge-test-mb"
DEL_LABEL = "jmap-bridge-test-del"
# 8080 matches the OAuth client's registered redirect URI.
BRIDGE = "http://127.0.0.1:8080/gmail"
TOKEN = "dev-token-gmail-local-only-0123456789"
AUTH = "Basic " + base64.b64encode(("any:" + TOKEN).encode()).decode()
SUBJECT = "jmap-bridge M11 write gate"
DRAFT_SUBJECT = "jmap-bridge M11 draft gate"

FOUND_LABELS = set()  # test labels seen, for cleanup


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
    address = None
    cfg = os.path.join(REPO, "dev/config-gmail.toml")
    if os.path.exists(cfg):
        for line in open(cfg):
            s = line.strip()
            if s.startswith("client_id"):
                client_id = s.split("=", 1)[1].strip().strip('"')
            elif s.startswith("address"):
                address = s.split("=", 1)[1].strip().strip('"')
    if client_id:
        os.environ.setdefault("GMAIL_TOKEN_CLIENT_ID", client_id)
    if address:
        os.environ.setdefault("GMAIL_ADDRESS", address)


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


def api(method, path, body=None, token=None, allow_404=False):
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
        if e.code == 404 and allow_404:
            return {}
        if e.code == 429:
            fail(f"Gmail API throttled the write gate (HTTP 429) — stopping, never loop", 2)
        raise


def jmap(method, args, callid):
    req = urllib.request.Request(
        BRIDGE + "/jmap",
        data=json.dumps({"using": ["urn:ietf:params:jmap:core",
                                   "urn:ietf:params:jmap:mail"],
                         "methodCalls": [[method, args, callid]]}).encode(),
        method="POST")
    req.add_header("Authorization", AUTH)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read())["methodResponses"][0]
    except urllib.error.HTTPError as e:
        if e.code == 429:
            fail("bridge returned HTTP 429 (throttled) — stopping", 2)
        body = e.read().decode(errors="replace")
        raise RuntimeError(f"JMAP HTTP {e.code}: {body[:500]}") from None


def throttle_guard(name, resp):
    """Abort rather than loop if the provider throttled a write."""
    for key in ("notCreated", "notUpdated", "notDestroyed"):
        for se in (resp.get(key) or {}).values():
            text = json.dumps(se).lower()
            if any(s in text for s in ("rate", "quota", "429", "throttl")):
                fail(f"{name} throttled: {se} — stopping, never loop", 2)


def fail(msg, code=1):
    print(f"FAIL: {msg}")
    sys.exit(code)


def ok(msg):
    print(f"PASS {msg}")


def write_bridge_config():
    os.makedirs(DATA, exist_ok=True)
    address = os.environ.get("GMAIL_ADDRESS", "")
    if not address:
        sys.exit("GMAIL_ADDRESS is not set (add address = ... to dev/config-gmail.toml)")
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
address = "{address}"
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
    return subprocess.Popen(["/tmp/opencode/jmap-bridge-gapi", "--config",
                             "/tmp/opencode/gmailapi-live.toml"],
                            stdout=log, stderr=subprocess.STDOUT, cwd=REPO,
                            env={**os.environ,
                                 "JMAP_BRIDGE_SECRET_KEY": os.environ["JMAP_BRIDGE_SECRET_KEY"]})


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


def gmail_labels(msg_id, token):
    m = api("GET", f"/messages/{msg_id}?format=metadata&metadataHeaders=Subject", token=token)
    return set(m.get("labelIds", [])), m.get("labelIds", [])


def bridge_mailboxes():
    r = jmap("Mailbox/get", {"accountId": "gmail"}, "m")[1]["list"]
    return {m["id"]: m for m in r}


def find_email(subject):
    q = jmap("Email/query", {"accountId": "gmail"}, "q")[1]["ids"]
    if not q:
        return None
    g = jmap("Email/get", {"accountId": "gmail", "ids": q,
                           "properties": ["id", "subject", "keywords", "mailboxIds"]}, "g")[1]
    for e in g["list"]:
        if e.get("subject") == subject:
            return e
    return None


PREFIXES = ("jmap-bridge-test",)
GATE_MSGID_PREFIX = "<m11-gate-"


def cleanup(token, msg_id):
    """Delete only what this gate mints: jmap-bridge-test* labels, the one
    test message, and drafts whose subject carries the gate prefix. Never
    touch any other draft, message or label."""
    if msg_id:
        api("DELETE", f"/messages/{msg_id}?permanent=true", token=token, allow_404=True)
    for l in api("GET", "/labels", token=token).get("labels", []):
        if l["name"].startswith(PREFIXES):
            api("DELETE", f"/labels/{l['id']}", token=token, allow_404=True)
    for d in api("GET", "/drafts", token=token).get("drafts", []):
        mid = d["message"]["id"]
        m = api("GET", f"/messages/{mid}?format=metadata&metadataHeaders=Subject",
                token=token, allow_404=True)
        if not m:
            continue
        subj = next((h["value"] for h in m.get("payload", {}).get("headers", [])
                     if h["name"].lower() == "subject"), "")
        if subj.startswith(DRAFT_SUBJECT):
            api("DELETE", f"/drafts/{d['id']}", token=token, allow_404=True)


def main():
    load_env()
    build()
    write_bridge_config()

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
        print("  open http://127.0.0.1:8080/oauth/gmail/start")
        sys.exit(3)

    p = start_bridge()
    token = None
    msg_id = None
    try:
        if not wait(session_ok, 30, "bridge session"):
            fail("bridge never served a session")
        ok("session served over API mode")
        token = access_token()

        labels = api("GET", "/labels", token=token).get("labels", [])
        existing = next((l for l in labels if l["name"] == LABEL), None)
        if existing:
            label_id = existing["id"]
        else:
            label_id = api("POST", "/labels", {"name": LABEL}, token=token)["id"]
            inbox_label_created = True
        ok(f"test label {LABEL}:{label_id}")

        address = os.environ["GMAIL_ADDRESS"]
        run = str(int(time.time()))
        subject = f"{SUBJECT} {run}"
        draft_subject = f"{DRAFT_SUBJECT} {run}"
        msgid = f"<m11-gate-{run}@jmap-bridge.test>"
        raw = (f"From: {address}\r\nTo: {address}\r\n"
               f"Subject: {subject}\r\nMessage-ID: {msgid}\r\n"
               f"Date: Mon, 02 Jan 2026 15:04:05 -0700\r\nMIME-Version: 1.0\r\n"
               f"Content-Type: text/plain; charset=utf-8\r\n\r\nM11 write gate body\r\n").encode()
        # Put it in INBOX *and* the test label so archive is meaningful.
        msg = api("POST", "/messages?internalDateSource=dateHeader",
                  {"raw": base64.urlsafe_b64encode(raw).rstrip(b"=").decode(),
                   "labelIds": ["INBOX", label_id]}, token=token)
        msg_id = msg["id"]
        ok(f"test message inserted: {msg_id}")

        e = None
        deadline = time.time() + 60
        while time.time() < deadline:
            e = find_email(subject)
            if e:
                break
            time.sleep(1)
        if not e:
            fail("test message never appeared in the bridge")
        eid = e["id"]
        ok(f"Email/query + Email/get returned {subject!r}")

        mbs = bridge_mailboxes()
        inbox_id = next((i for i, m in mbs.items() if m.get("role") == "inbox"), None)
        drafts_id = next((i for i, m in mbs.items() if m.get("role") == "drafts"), None)
        if not wait(lambda: any(m.get("name") == LABEL for m in bridge_mailboxes().values()),
                    30, "test label mailbox"):
            names = sorted(m.get("name") for m in bridge_mailboxes().values())
            fail(f"test label mailbox {LABEL} never appeared; mailboxes: {names}")
        mbs = bridge_mailboxes()
        label_mbx_id = next((i for i, m in mbs.items() if m.get("name") == LABEL), None)
        if not (inbox_id and drafts_id and label_mbx_id):
            fail(f"mailboxes missing: inbox={inbox_id} drafts={drafts_id} label={label_mbx_id}")

        # --- star (keywords/$flagged) ---
        resp = jmap("Email/set", {"accountId": "gmail",
                                  "update": {eid: {"keywords/$flagged": True}}}, "s")[1]
        throttle_guard("star", resp)
        if eid not in (resp.get("updated") or {}):
            fail(f"star not updated: {resp}")
        if not wait(lambda: "STARRED" in gmail_labels(msg_id, token)[0], 30, "STARRED"):
            fail("bridge star did not land on Gmail")
        ok("star: bridge → Gmail STARRED, re-read independently")

        # --- mark read (keywords/$seen, inverse of UNREAD) ---
        resp = jmap("Email/set", {"accountId": "gmail",
                                  "update": {eid: {"keywords/$seen": True}}}, "s")[1]
        throttle_guard("seen", resp)
        if eid not in (resp.get("updated") or {}):
            fail(f"seen not updated: {resp}")
        if not wait(lambda: "UNREAD" not in gmail_labels(msg_id, token)[0], 30, "unread cleared"):
            fail("bridge $seen did not clear UNREAD on Gmail")
        ok("read: keywords/$seen → Gmail UNREAD removed, re-read independently")

        # --- archive (remove INBOX membership only) ---
        resp = jmap("Email/set", {"accountId": "gmail",
                                  "update": {eid: {f"mailboxIds/{inbox_id}": None}}}, "s")[1]
        throttle_guard("archive", resp)
        if eid not in (resp.get("updated") or {}):
            fail(f"archive not updated: {resp}")
        if not wait(lambda: "INBOX" not in gmail_labels(msg_id, token)[0], 30, "INBOX gone"):
            fail("archive did not drop INBOX on Gmail")
        ok("archive: INBOX membership removed, message stays in All Mail/test label")

        # --- Mailbox/set create + move, then rename ---
        resp = jmap("Mailbox/set", {"accountId": "gmail",
                                    "create": {"mb": {"name": MOVE_LABEL}}}, "s")[1]
        throttle_guard("mailbox create", resp)
        move_id = (resp.get("created") or {}).get("mb", {}).get("id")
        if not move_id:
            fail(f"Mailbox/set create failed: {resp}")
        if not wait(lambda: any(l["name"] == MOVE_LABEL for l in api("GET", "/labels", token=token)["labels"]),
                    30, "move label"):
            fail("bridge-created label not on Gmail")
        gmove_id = next(l["id"] for l in api("GET", "/labels", token=token)["labels"] if l["name"] == MOVE_LABEL)
        ok(f"Mailbox/set create → Gmail label {MOVE_LABEL}")

        resp = jmap("Email/set", {"accountId": "gmail",
                                  "update": {eid: {f"mailboxIds/{move_id}": True}}}, "s")[1]
        throttle_guard("move", resp)
        if not wait(lambda: gmove_id in gmail_labels(msg_id, token)[0], 30, "moved"):
            fail("move did not add the label on Gmail")
        ok("move: mailbox add → Gmail label added, re-read independently")

        resp = jmap("Mailbox/set", {"accountId": "gmail",
                                    "update": {move_id: {"name": MAILBOX}}}, "s")[1]
        throttle_guard("mailbox rename", resp)
        if not wait(lambda: any(l["name"] == MAILBOX for l in api("GET", "/labels", token=token)["labels"]),
                    30, "renamed"):
            fail("Mailbox/set rename not on Gmail")
        ok(f"Mailbox/set rename → Gmail label {MAILBOX}")

        # --- refusing onDestroyRemoveEmails=true ---
        resp = jmap("Mailbox/set", {"accountId": "gmail", "destroy": [move_id],
                                    "onDestroyRemoveEmails": True}, "s")[1]
        nd = (resp.get("notDestroyed") or {}).get(move_id, {})
        if nd.get("type") != "invalidProperties" or "onDestroyRemoveEmails" not in nd.get("properties", []):
            fail(f"onDestroyRemoveEmails=true not refused as invalidProperties: {resp}")
        ok("Mailbox/set onDestroyRemoveEmails=true refused (invalidProperties)")

        # --- Mailbox/set destroy (false) on an empty label ---
        # delete a label that holds no messages, so the outcome is not
        # perturbed by Gmail's eventually-consistent label counts. (A
        # non-empty label with removeEmails=false answers mailboxHasEmail
        # — covered by unit tests; asserting it live would race the
        # count re-derivation.)
        resp = jmap("Mailbox/set", {"accountId": "gmail",
                                    "create": {"del": {"name": DEL_LABEL}}}, "s")[1]
        throttle_guard("mailbox create (del)", resp)
        del_id = (resp.get("created") or {}).get("del", {}).get("id")
        if not del_id:
            fail(f"Mailbox/set create (del) failed: {resp}")
        if not wait(lambda: any(l["name"] == DEL_LABEL for l in api("GET", "/labels", token=token)["labels"]),
                    30, "delete label"):
            fail("bridge-created delete label not on Gmail")
        resp = jmap("Mailbox/set", {"accountId": "gmail", "destroy": [del_id],
                                    "onDestroyRemoveEmails": False}, "s")[1]
        throttle_guard("mailbox destroy", resp)
        if del_id not in (resp.get("destroyed") or []):
            fail(f"Mailbox/set destroy failed: {resp}")
        if not wait(lambda: not any(l["name"] == DEL_LABEL for l in api("GET", "/labels", token=token)["labels"]),
                    30, "label deleted"):
            fail("Mailbox/set destroy did not remove the Gmail label")
        ok("Mailbox/set destroy → Gmail label deleted")

        # --- draft create ---
        resp = jmap("Email/set", {"accountId": "gmail", "create": {"d1": {
            "mailboxIds": {drafts_id: True}, "subject": draft_subject,
            "bodyValues": {"p1": {"value": "draft gate body"}},
            "textBody": [{"partId": "p1", "type": "text/plain"}],
        }}}, "s")[1]
        throttle_guard("draft create", resp)
        if "d1" not in (resp.get("created") or {}):
            fail(f"draft create failed: {resp}")
        ok("Email/set create → draft created")

        def draft_visible():
            for d in api("GET", "/drafts", token=token).get("drafts", []):
                mid = d["message"]["id"]
                m = api("GET", f"/messages/{mid}?format=metadata&metadataHeaders=Subject", token=token)
                subj = next((h["value"] for h in m.get("payload", {}).get("headers", [])
                             if h["name"].lower() == "subject"), None)
                if subj == draft_subject:
                    return True
            return False

        if not wait(draft_visible, 30, "draft on Gmail"):
            fail("bridge draft never appeared in Gmail drafts")
        ok("draft re-read independently through Gmail drafts.list/get")

        # --- destroy (permanent) ---
        resp = jmap("Email/set", {"accountId": "gmail", "destroy": [eid]}, "s")[1]
        throttle_guard("destroy", resp)
        if eid not in (resp.get("destroyed") or []):
            fail(f"destroy failed: {resp}")
        if not wait(lambda: not api("GET", f"/messages/{msg_id}", token=token, allow_404=True),
                    30, "message gone"):
            fail("destroy did not remove the message on Gmail")
        msg_id = None  # nothing left to clean up
        ok("destroy: message permanently gone from Gmail, re-read independently")
    finally:
        p.terminate()
        try:
            p.wait(timeout=10)
        except Exception:
            p.kill()
        try:
            if token:
                cleanup(token, msg_id)
            print("PASS cleaned up the test objects")
        except Exception as ex:  # noqa: BLE001
            print(f"WARN cleanup: {ex}")


if __name__ == "__main__":
    main()
