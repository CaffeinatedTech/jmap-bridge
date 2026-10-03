#!/usr/bin/env python3
"""M12 live send gate — Gmail API mode submission against a Gmail account.

Self-send variant: it composes one draft through the bridge, submits it with
``EmailSubmission/set`` (the caller's ``onSuccessUpdateEmail`` moving it from
Drafts to Sent), and then re-reads the Gmail API **independently** to prove

  * exactly one message with the SENT label (Gmail files its own copy; the
    bridge must never APPEND a second one), and
  * the message was actually delivered — a copy with the INBOX label, because
    the sink is the account's own address.

It also proves the draft was consumed by ``drafts.send`` (no message with the
DRAFT label and the gate subject) and that the bridge's own Sent mailbox holds
exactly one copy (read-your-writes).

It only ever touches messages whose subject it mints, and it aborts on the
first ``429`` rather than retrying, so a run can never deepen a throttle.
Secrets stay in ``.env`` (0600) and are never printed. Backfill is scoped by
the gate subject so the bridge caches only its own test messages.
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
# 8080 matches the OAuth client's registered redirect URI.
BRIDGE = "http://127.0.0.1:8080/gmail"
TOKEN = "dev-token-gmail-local-only-0123456789"
AUTH = "Basic " + base64.b64encode(("any:" + TOKEN).encode()).decode()
SUBJECT = "jmap-bridge M12 send gate"


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
            fail("Gmail API throttled the send gate (HTTP 429) — stopping, never loop", 2)
        raise


def jmap(method, args, callid):
    req = urllib.request.Request(
        BRIDGE + "/jmap",
        data=json.dumps({"using": ["urn:ietf:params:jmap:core",
                                   "urn:ietf:params:jmap:mail",
                                   "urn:ietf:params:jmap:submission"],
                         "methodCalls": [[method, args, callid]]}).encode(),
        method="POST")
    req.add_header("Authorization", AUTH)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.loads(r.read())["methodResponses"][0]
    except urllib.error.HTTPError as e:
        if e.code == 429:
            fail("bridge returned HTTP 429 (throttled) — stopping", 2)
        body = e.read().decode(errors="replace")
        raise RuntimeError(f"JMAP HTTP {e.code}: {body[:500]}") from None


def jmap_batch(calls):
    """Run several method calls in one request (compose + submit)."""
    req = urllib.request.Request(
        BRIDGE + "/jmap",
        data=json.dumps({"using": ["urn:ietf:params:jmap:core",
                                   "urn:ietf:params:jmap:mail",
                                   "urn:ietf:params:jmap:submission"],
                         "methodCalls": calls}).encode(),
        method="POST")
    req.add_header("Authorization", AUTH)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return json.loads(r.read())["methodResponses"]
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
  backfill_query = "subject:{SUBJECT}"
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
        time.sleep(1)
    return False


def session_ok():
    try:
        req = urllib.request.Request(BRIDGE + "/.well-known/jmap")
        req.add_header("Authorization", AUTH)
        with urllib.request.urlopen(req, timeout=5) as r:
            d = json.loads(r.read())
        return "urn:ietf:params:jmap:submission" in d.get("capabilities", {})
    except Exception:
        return False


def bridge_mailboxes():
    r = jmap("Mailbox/get", {"accountId": "gmail"}, "m")[1]["list"]
    return {m["id"]: m for m in r}


def gmail_search(token, query):
    """Return every message id matching a Gmail query (paginated)."""
    ids = []
    page = None
    while True:
        path = "/messages?maxResults=100&q=" + urllib.parse.quote(query)
        if page:
            path += "&pageToken=" + urllib.parse.quote(page)
        r = api("GET", path, token=token)
        ids += [m["id"] for m in r.get("messages", [])]
        page = r.get("nextPageToken")
        if not page:
            break
    return ids


def gmail_message(token, mid):
    return api("GET", f"/messages/{mid}?format=metadata"
                      "&metadataHeaders=Subject&metadataHeaders=Message-ID", token=token)


def gmail_subject(m):
    return next((h["value"] for h in m.get("payload", {}).get("headers", [])
                 if h["name"].lower() == "subject"), "")


def gate_messages(token, subject):
    """Every Gmail message whose exact subject is the gate's, with its labels."""
    out = []
    for mid in gmail_search(token, f'subject:"{subject}"'):
        m = gmail_message(token, mid)
        if gmail_subject(m) == subject:
            out.append((mid, set(m.get("labelIds", []))))
    return out


def bridge_email_by_subject(subject):
    q = jmap("Email/query", {"accountId": "gmail"}, "q")[1]["ids"]
    if not q:
        return None
    g = jmap("Email/get", {"accountId": "gmail", "ids": q,
                           "properties": ["id", "subject", "keywords", "mailboxIds"]}, "g")[1]
    for e in g["list"]:
        if e.get("subject") == subject:
            return e
    return None


def cleanup(token, subject):
    """Delete only the messages this gate minted (its exact subject)."""
    for mid, _ in gate_messages(token, subject):
        api("DELETE", f"/messages/{mid}?permanent=true", token=token, allow_404=True)
    # A failed submission can leave the draft behind; retire only our subject.
    for d in api("GET", "/drafts", token=token).get("drafts", []):
        m = api("GET", f"/messages/{d['message']['id']}?format=metadata&metadataHeaders=Subject",
                token=token, allow_404=True)
        if m and gmail_subject(m) == subject:
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
    address = os.environ["GMAIL_ADDRESS"]
    run = str(int(time.time()))
    subject = f"{SUBJECT} {run}"
    try:
        if not wait(session_ok, 30, "bridge session"):
            fail("bridge never served a session with the submission capability")
        ok("session served over API mode with urn:ietf:params:jmap:submission")
        token = access_token()

        ident = jmap("Identity/get", {"accountId": "gmail"}, "i")[1]["list"]
        if not ident:
            fail("Identity/get returned no identity")
        identity_id = ident[0]["id"]
        ok(f"Identity/get → {ident[0]['email']}")

        mbs = bridge_mailboxes()
        drafts_id = next((i for i, m in mbs.items() if m.get("role") == "drafts"), None)
        sent_id = next((i for i, m in mbs.items() if m.get("role") == "sent"), None)
        if not (drafts_id and sent_id):
            fail(f"mailbox roles missing: drafts={drafts_id} sent={sent_id}")

        # --- compose + submit in one batch (the composing-client shape) ---
        calls = [
            ["Email/set", {"accountId": "gmail", "create": {"d": {
                "mailboxIds": {drafts_id: True},
                "keywords": {"$draft": True, "$seen": True},
                "from": [{"email": address}],
                "to": [{"email": address}],  # self-send: the sink is ourselves
                "subject": subject,
                "bodyValues": {"p1": {"value": "M12 send gate body"}},
                "textBody": [{"partId": "p1", "type": "text/plain"}],
            }}}, "0"],
            ["EmailSubmission/set", {"accountId": "gmail",
                "create": {"sub": {"identityId": identity_id, "emailId": "#d"}},
                "onSuccessUpdateEmail": {"#sub": {
                    f"mailboxIds/{drafts_id}": None,
                    f"mailboxIds/{sent_id}": True,
                    "keywords/$draft": None,
                }}}, "1"],
        ]
        out = jmap_batch(calls)
        created = (out[0][1].get("created") or {}).get("d")
        if not created:
            fail(f"draft create failed: {out[0][1]}")
        draft_id = created["id"]
        ok(f"Email/set create → draft {draft_id}")

        sub = out[1][1]
        throttle_guard("submission", sub)
        entry = (sub.get("created") or {}).get("sub")
        if not entry:
            fail(f"EmailSubmission/set failed: {sub}")
        if entry.get("undoStatus") != "final":
            fail(f"undoStatus = {entry.get('undoStatus')}, want final")
        ok("EmailSubmission/set create → undoStatus final")
        if len(out) >= 3 and out[2][0] == "Email/set":
            if draft_id in (out[2][1].get("updated") or {}):
                ok("implicit Email/set moved the draft (RFC 8621 §7.5)")
            else:
                fail(f"implicit Email/set did not update the draft: {out[2][1]}")

        # --- independently: exactly one SENT copy, delivered to INBOX ---
        def snapshot():
            return gate_messages(token, subject)

        if not wait(lambda: len(snapshot()) >= 1, 60, "message in Gmail"):
            fail("the submitted message never appeared in Gmail")
        msgs = snapshot()
        sent = [mid for mid, labels in msgs if "SENT" in labels]
        inbox = [mid for mid, labels in msgs if "INBOX" in labels]
        drafts = [mid for mid, labels in msgs if "DRAFT" in labels]
        if len(sent) != 1:
            fail(f"Gmail holds {len(sent)} SENT copies, want exactly 1: {msgs}")
        if drafts:
            fail(f"Gmail still holds {len(drafts)} DRAFT copies, want 0 (drafts.send consumed it)")
        if not wait(lambda: len([m for m, l in snapshot() if "INBOX" in l]) >= 1, 60, "delivery to INBOX"):
            fail("the self-send never arrived in INBOX (delivery unproven)")
        ok("Gmail independently: exactly one SENT copy, INBOX delivery, no draft")

        # --- the bridge's own cache agrees: one Sent copy, no longer a draft ---
        def bridge_sent():
            e = bridge_email_by_subject(subject)
            if not e:
                return False
            return sent_id in e["mailboxIds"] and drafts_id not in e["mailboxIds"] \
                and not e["keywords"].get("$draft")
        if not wait(bridge_sent, 60, "bridge Sent mailbox"):
            fail(f"bridge did not record the Sent copy read-your-writes: "
                 f"{bridge_email_by_subject(subject)}")
        sent_emails = [e for e in [bridge_email_by_subject(subject)] if e]
        if len(sent_emails) != 1:
            fail(f"bridge holds {len(sent_emails)} copies, want 1")
        ok("bridge cache: one Sent copy, $draft cleared (read-your-writes)")

        print(f"\nM12 live send gate green: compose → send → 1 SENT + INBOX delivery (self-send), "
              f"subject {subject!r}")
    finally:
        p.terminate()
        try:
            p.wait(timeout=10)
        except Exception:
            p.kill()
        try:
            if token:
                cleanup(token, subject)
            print("PASS cleaned up the gate's messages")
        except Exception as ex:  # noqa: BLE001
            print(f"WARN cleanup: {ex}")


if __name__ == "__main__":
    main()
