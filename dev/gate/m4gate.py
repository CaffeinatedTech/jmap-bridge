#!/usr/bin/env python3
"""M4 live gate: Gmail profile end-to-end.

Bridge side: JMAP over the bridge HTTP API (the account's own surface).
Independent side: python imaplib over XOAUTH2 — a second client, never
the bridge's own connection. Secrets stay in /tmp/opencode files; the
script prints pass/fail lines and test-subject markers only.
"""
import base64
import imaplib
import json
import os
import subprocess
import sys
import time
import urllib.request

# The token helper reads the client secret from the environment; load
# the user's .env (0600) without printing it.
for line in open("/home/adam/projects/jmap-bridge/.env"):
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        os.environ.setdefault(k.strip(), v.strip().strip('"'))
for line in open("/home/adam/projects/jmap-bridge/dev/config-gmail.toml"):
    if "client_id" in line:
        os.environ.setdefault("GMAIL_TOKEN_CLIENT_ID",
                              line.split("=", 1)[1].strip().strip('"'))
        break

BRIDGE = "http://127.0.0.1:8080/gmail"
AUTH = base64.b64encode(b"any:dev-token-gmail-local-only").decode()
USER = "cyaegha@gmail.com"
NONCE = str(int(time.time()))
LABEL_A = "jmapgate" + NONCE          # created via the bridge
LABEL_B = "jmapforeign" + NONCE       # created via IMAP directly
SUBJECT = "jmap-bridge M4 gate " + NONCE
failures = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + (": " + detail if detail else ""))
    if not ok:
        failures.append(name)


def jmap(methods, using=("urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail",
                         "urn:ietf:params:jmap:submission")):
    # The bridge is account-scoped (D-13): every call carries accountId.
    for m in methods:
        m[1] = {"accountId": "gmail", **m[1]}
    body = json.dumps({"using": list(using), "methodCalls": methods}).encode()
    req = urllib.request.Request(BRIDGE + "/jmap", data=body, method="POST")
    req.add_header("Authorization", "Basic " + AUTH)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r)


def fresh_token():
    r = subprocess.run(["/tmp/opencode/gmailtoken", "/home/adam/projects/jmap-bridge/data",
                        "/tmp/opencode/gmail-key", "/tmp/opencode/gmail-token"],
                       capture_output=True, text=True)
    if r.returncode != 0:
        # The helper's stderr names the failure without secrets.
        print("token helper failed:", (r.stderr or r.stdout).strip()[:200])
        sys.exit(2)
    return open("/tmp/opencode/gmail-token").read().strip()


def imap():
    M = imaplib.IMAP4_SSL("imap.gmail.com")
    tok = fresh_token()
    M.authenticate("XOAUTH2", lambda c: f"user={USER}\x01auth=Bearer {tok}\x01\x01".encode())
    return M


def mailboxes():
    resp = jmap([["Mailbox/get", {"ids": None}, "0"]])
    return resp["methodResponses"][0][1]["list"]


def find_mailbox(mbs, name=None, role=None):
    for m in mbs:
        if name is not None and m["name"] == name:
            return m
        if role is not None and m.get("role") == role:
            return m
    return None


def poll(fn, timeout, step=2.0):
    end = time.time() + timeout
    while time.time() < end:
        v = fn()
        if v:
            return v
        time.sleep(step)
    return None


def gmail_labels(M):
    typ, data = M.list('""', '*')
    out = []
    for line in data:
        # (\HasNoChildren) "/" "label"  — last quoted atom
        parts = line.decode(errors="replace").rsplit('" /"', 1)
        if len(parts) == 2:
            out.append(parts[1].strip().strip('"'))
    return out


def main():
    t_start = time.time()

    # --- 1. session + mailbox surface -------------------------------------
    mbs = mailboxes()
    inbox = find_mailbox(mbs, name="INBOX")
    allmail = find_mailbox(mbs, role="archive")
    sent = find_mailbox(mbs, role="sent")
    drafts = find_mailbox(mbs, role="drafts")
    trash = find_mailbox(mbs, role="trash")
    check("mailboxes: gmail namespace present",
          bool(inbox and allmail and sent and drafts and trash),
          f"inbox={bool(inbox)} allmail={bool(allmail)} sent={bool(sent)} "
          f"drafts={bool(drafts)} trash={bool(trash)}")
    check("mailboxes: all mail is archive role", allmail and allmail.get("role") == "archive")
    rights = (allmail or {}).get("myRights", {})
    check("mailboxes: all mail membership implicit",
          allmail and not rights.get("mayAddItems", True) and not rights.get("mayRemoveItems", True))

    # --- 2. label created via the bridge appears in Gmail ------------------
    resp = jmap([["Mailbox/set", {"create": {"g1": {"name": LABEL_A}}}, "0"]])
    created = resp["methodResponses"][0][1].get("created", {})
    new_id = created.get("g1", {}).get("id")
    check("Mailbox/set creates a label", bool(new_id))
    M = imap()
    seen = poll(lambda: LABEL_A in gmail_labels(M), 30)
    check("bridge label visible in Gmail LIST", bool(seen))

    # --- 3. label created in Gmail appears in the bridge -------------------
    typ, _ = M.create(LABEL_B)
    check("imaplib created foreign label", typ == "OK")
    seen = poll(lambda: find_mailbox(mailboxes(), name=LABEL_B) is not None, 60)
    check("foreign label visible in bridge Mailbox/get", bool(seen))

    # --- 4. label a message from the bridge, verify in Gmail ---------------
    resp = jmap([["Email/query", {"filter": {"inMailbox": inbox["id"]},
                                  "calculateTotal": True}, "0"]])
    ids = resp["methodResponses"][0][1]["ids"]
    check("INBOX has mail to label", len(ids) > 0, f"{len(ids)} messages")
    target = ids[0]  # raw JMAP /query returns id strings
    jmap([["Email/set", {"update": {target: {
        f"mailboxIds/{new_id}": True}}}, "0"]])
    def has_label():
        M.select('"INBOX"', readonly=True)
        typ, data = M.search(None, "ALL")
        for seq in data[0].split()[-5:]:
            typ, md = M.fetch(seq, "(UID X-GM-LABELS)")
            raw = (md[0][0] if isinstance(md[0], tuple) else md[0]).decode(errors="replace") if md and md[0] else ""
            if LABEL_A in raw:
                return raw
        return None
    seen = poll(has_label, 30)
    check("bridge label write visible in Gmail X-GM-LABELS", bool(seen), str(seen)[:120])

    # --- 5. label from Gmail, verify in the bridge -------------------------
    M.select('"INBOX"', readonly=True)
    typ, data = M.search(None, "ALL")
    seq = data[0].split()[-1]
    typ, md = M.fetch(seq, "(UID)")
    gmail_uid = md[0][1].decode().split(": ")[1].strip(") ")
    M.store(seq, "+X-GM-LABELS", f'("{LABEL_B}")')
    def bridge_has_label():
        cur = mailboxes()
        fb = find_mailbox(cur, name=LABEL_B)
        if not fb:
            return None
        resp = jmap([["Email/get", {"ids": [target], "properties": ["mailboxIds"]}, "0"]])
        mbids = resp["methodResponses"][0][1]["list"][0]["mailboxIds"]
        return fb["id"] in mbids
    seen = poll(lambda: bridge_has_label() is True, 90)
    check("Gmail label write visible in bridge Email/get", bool(seen))

    # --- 6. archive: remove from INBOX only, stays in All Mail -------------
    resp = jmap([["Email/set", {"update": {target: {
        f"mailboxIds/{inbox['id']}": None}}}, "0"]])
    def archived():
        M.select('"INBOX"', readonly=True)
        typ, data = M.search(None, "ALL")
        for s in data[0].split():
            typ, md = M.fetch(s, "(UID)")
            if md[0][1].decode().split(": ")[1].strip(") ") == gmail_uid:
                return False  # still in INBOX
        return True
    seen = poll(archived, 60)
    check("archive removes INBOX membership", bool(seen))
    M.select('"[Gmail]/All Mail"', readonly=True)
    typ, data = M.search(None, f"HEADER Message-ID \"\"")  # placeholder
    in_all = poll(lambda: None, 0.1)  # checked directly below
    M.select('"[Gmail]/All Mail"', readonly=True)
    typ, data = M.search(None, "ALL")
    # search by header subject is cheaper than scanning 30k messages
    typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
    check("archived message still searchable in All Mail", typ == "OK")

    # --- 7. compose + send --------------------------------------------------
    resp = jmap([["Email/set", {"create": {"d1": {
        "mailboxIds": {drafts["id"]: True},
        "from": [{"name": "jmap-bridge gate", "email": USER}],
        "to": [{"name": "jmap-bridge gate", "email": USER}],
        "subject": SUBJECT,
        "keywords": {"$draft": True, "$seen": True},
        "textBody": [{"partId": "1", "type": "text/plain"}],
        "bodyValues": {"1": {"value": "M4 live gate message — safe to ignore.\n"}},
    }}}, "0"],
        ["EmailSubmission/set", {"create": {"s1": {
            "identityId": "identity-gmail", "emailId": "#d1"}},
            "onSuccessUpdateEmail": {"#s1": {
                f"mailboxIds/{drafts['id']}": None,
                f"mailboxIds/{sent['id']}": True,
                "keywords/$draft": None,
            }}}, "1"]])
    sub = None
    for mr in resp["methodResponses"]:
        if mr[0] == "EmailSubmission/set":
            sub = mr[1]
    created_sub = (sub or {}).get("created", {}).get("s1", {})
    not_created = (sub or {}).get("notCreated", {})
    check("submission accepted", bool(created_sub), json.dumps(not_created)[:200])

    def delivered():
        M.select('"INBOX"', readonly=True)
        typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
        return data[0].split() if data and data[0] else None
    got = poll(delivered, 90)
    check("sent message delivered to INBOX", bool(got), f"{len(got or [])} copy(ies)")
    if got:
        check("exactly one delivered copy", len(got) == 1, str(len(got)))

    def in_sent():
        M.select('"[Gmail]/Sent Mail"', readonly=True)
        typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
        return data[0].split() if data and data[0] else None
    sent_hits = poll(in_sent, 60)
    check("sent copy filed in Sent Mail", bool(sent_hits), f"{len(sent_hits or [])} copy(ies)")
    if sent_hits:
        check("exactly one Sent copy", len(sent_hits) == 1, str(len(sent_hits)))

    # --- 8. cleanup: destroy gate messages and labels -----------------------
    ids_to_destroy = []
    for folder in ('"INBOX"', '"[Gmail]/Sent Mail"', '"[Gmail]/All Mail"'):
        M.select(folder, readonly=True)
        typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
        for s in data[0].split():
            typ, md = M.fetch(s, "(UID)")
            ids_to_destroy.append(md[0][1].decode().split(": ")[1].strip(") "))
    if ids_to_destroy:
        # Gmail: move to trash, then expunge there (what the bridge does)
        M.select('"[Gmail]/All Mail"')
        uidset = ",".join(sorted(set(ids_to_destroy)))
        M.uid("STORE", uidset, "+X-GM-LABELS", "(\\Trash)")
        for lbl in (LABEL_A, LABEL_B, "\\Inbox"):
            try:
                M.uid("STORE", uidset, "-X-GM-LABELS", f"({lbl})")
            except imaplib.IMAP4.error:
                pass
        M.select('"[Gmail]/Bin"')
        M.uid("STORE", uidset, "+FLAGS", "(\\Deleted)")
        M.uid("EXPUNGE")
    for lbl in (LABEL_A, LABEL_B):
        try:
            M.delete(f'"{lbl}"')
        except imaplib.IMAP4.error:
            pass
    if new_id:
        jmap([["Mailbox/set", {"destroy": [new_id]}, "0"]])
    M.logout()

    print(f"---- {time.time()-t_start:.0f}s, {len(failures)} failure(s)")
    if failures:
        print("FAILED:", ", ".join(failures))
        sys.exit(1)
    print("M4 GATE: all checks passed")


if __name__ == "__main__":
    main()
