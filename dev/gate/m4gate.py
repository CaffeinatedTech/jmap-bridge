#!/usr/bin/env python3
"""M4 live gate — the Gmail-specific deltas that were still unproven.

The read surface, OAuth2/XOAUTH2 IMAP, the Gmail namespace and roles,
implicit All Mail, label create/list both ways and the bridge->Gmail
message-label write were demonstrated by the 2026-10-01 partial runs and
are covered by the fixture tiers. This gate re-checks only what those runs
never got to:

  1. a message label applied in Gmail appears in the bridge;
  2. archiving from the bridge drops INBOX membership (and is undone, so no
     mail is left moved);
  3. compose + send over Gmail SMTP (XOAUTH2) files one Sent copy and
     delivers one copy.

Gmail quarantines IMAP writes per account after sustained use and then drops
them with `OK [THROTTLED]`. This gate ABORTS on the first sign of it (and
paces its few writes) so a run can never deepen the quarantine: if it
aborts, stop every IMAP writer on the account and wait ~24 h, then run
once more. It is a one-shot sign-off, not a loop.

Bridge side: JMAP over the bridge HTTP API (the account's own surface).
Independent side: python imaplib over XOAUTH2 — a second client, never the
bridge's own connection. Secrets stay in /tmp/opencode files; only pass/fail
lines and test-subject markers are printed.
"""
import base64
import imaplib
import json
import os
import subprocess
import sys
import time
import urllib.request

# The token helper reads the client secret from the environment; load the
# user's .env (0600) without printing it.
for line in open("/home/adam/projects/jmap-bridge/.env"):
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        os.environ.setdefault(k.strip(), v.strip().strip('"'))
keyfile = os.environ.get("GMAIL_KEY_FILE", "/tmp/opencode/gmail-key")
if not os.path.exists(keyfile):
    for line in open("/home/adam/projects/jmap-bridge/.env"):
        if line.startswith("JMAP_BRIDGE_SECRET_KEY="):
            fd = os.open(keyfile, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            os.write(fd, line.split("=", 1)[1].strip().encode())
            os.close(fd)
            break
for line in open("/home/adam/projects/jmap-bridge/dev/config-gmail.toml"):
    if "client_id" in line:
        os.environ.setdefault("GMAIL_TOKEN_CLIENT_ID",
                              line.split("=", 1)[1].strip().strip('"'))
        break

BRIDGE = "http://127.0.0.1:8080/gmail"
AUTH = base64.b64encode(b"any:dev-token-gmail-local-only").decode()
USER = "you@gmail.com"
NONCE = str(int(time.time()))
LABEL = "jmapgate" + NONCE          # applied in Gmail, expected in the bridge
SUBJECT = "jmap-bridge M4 gate " + NONCE
WRITE_PACE = 20                     # seconds between write groups
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


def method(resp, name):
    for mr in resp["methodResponses"]:
        if mr[0] == name:
            return mr[1]
    return {}


def fresh_token():
    helper = "/tmp/opencode/gmailtoken"
    if not os.path.exists(helper):
        b = subprocess.run(["go", "build", "-o", helper, "./test/live/gmailtoken"],
                           cwd="/home/adam/projects/jmap-bridge",
                           capture_output=True, text=True)
        if b.returncode != 0:
            print("helper build failed:", (b.stderr or b.stdout).strip()[:200])
            sys.exit(2)
    r = subprocess.run([helper, "/home/adam/projects/jmap-bridge/data",
                        os.environ.get("GMAIL_KEY_FILE", "/tmp/opencode/gmail-key"),
                        "/tmp/opencode/gmail-token"],
                       capture_output=True, text=True)
    if r.returncode != 0:
        print("token helper failed:", (r.stderr or r.stdout).strip()[:200])
        sys.exit(2)
    return open("/tmp/opencode/gmail-token").read().strip()


def imap():
    M = imaplib.IMAP4_SSL("imap.gmail.com")
    tok = fresh_token()
    M.authenticate("XOAUTH2", lambda c: f"user={USER}\x01auth=Bearer {tok}\x01\x01".encode())
    return M


def mailboxes():
    return jmap([["Mailbox/get", {"ids": None}, "0"]])["methodResponses"][0][1]["list"]


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
        # (\HasNoChildren) "/" "label"  — split on the quoted "/" delimiter.
        parts = line.decode(errors="replace").rsplit('"/"', 1)
        if len(parts) == 2:
            out.append(parts[1].strip().strip('"'))
    return out


def fetch_uid(M, seq):
    # imaplib hands back a FETCH "(UID)" value as an int on current Python
    # and as a bytes blob on older builds; normalise to a string.
    typ, md = M.fetch(seq, "(UID)")
    item = md[0]
    val = item[1] if isinstance(item, tuple) else item
    if isinstance(val, int):
        return str(val)
    return val.decode(errors="replace").split("UID")[-1].strip().strip(") ")


def fetch_msgid(M, seq):
    # X-GM-MSGID is Gmail's account-global message id: unlike a uid it is
    # the same in every folder the message lives in, so it survives the
    # new uid a message gets when it re-enters a folder.
    typ, md = M.fetch(seq, "(X-GM-MSGID)")
    item = md[0]
    val = item[1] if isinstance(item, tuple) else item
    s = val.decode(errors="replace") if isinstance(val, bytes) else str(val)
    return s.split("X-GM-MSGID")[-1].strip().strip(") ")


def stop_if_throttled(where, typ, data):
    # Any non-OK tagged response — and especially the [THROTTLED] code — means
    # Gmail did not perform the write. Send nothing further: retrying is what
    # keeps the quarantine alive.
    blob = b" ".join(x if isinstance(x, bytes) else str(x).encode()
                     for x in (data if isinstance(data, (list, tuple)) else [data]))
    if typ != "OK" or b"[THROTTLED]" in blob:
        print("ABORT at %s: %s %s" % (where, typ, blob[:120]))
        print("Gmail is throttling IMAP writes. Stopping all writers and waiting ~24 h.")
        sys.exit(4)


def pace():
    time.sleep(WRITE_PACE)


def has_label(M, seq, label):
    typ, md = M.fetch(seq, "(X-GM-LABELS)")
    raw = b""
    for item in md or []:
        raw += item if isinstance(item, bytes) else str(item).encode()
    return label.encode() in raw


def inbox_has_msgid(M, msgid):
    # Membership by X-GM-MSGID, not by the INBOX uid: a message that
    # leaves and re-enters INBOX (archive, then undo) gets a fresh uid,
    # so the uid captured before the archive would always read as gone.
    M.select('"INBOX"', readonly=True)
    typ, data = M.search(None, "X-GM-MSGID", msgid)
    return bool(data and data[0])


def main():
    t_start = time.time()

    # --- 0. read-only surface ---------------------------------------------
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
    check("mailboxes: all mail is archive role", bool(allmail and allmail.get("role") == "archive"))

    if not (inbox and allmail and sent and drafts):
        print("cannot continue without the core mailboxes")
        sys.exit(1)

    M = imap()
    identity = method(jmap([["Identity/get", {"ids": None}, "0"]]), "Identity/get").get("list", [])
    identity_id = identity[0]["id"] if identity else ""
    check("Identity/get returns an identity", bool(identity_id))

    # --- 1. label applied in Gmail appears in the bridge ------------------
    M.select('"INBOX"')  # writable: STORE is refused on a read-only folder
    typ, data = M.search(None, "ALL")
    seqs = data[0].split()
    check("INBOX has mail", bool(seqs))
    if not seqs:
        finish(M, t_start)
        return
    seq = seqs[-1]
    gmail_msgid = fetch_msgid(M, seq)
    stop_if_throttled("STORE +label", *M.store(seq, "+X-GM-LABELS", f'("{LABEL}")'))

    def bridge_label_target():
        # The STORE creates the label folder; once the bridge has discovered
        # it, the message under it is our seq. Going through the label gives
        # an exact IMAP-message ⇄ JMAP-id mapping, independent of any query
        # ordering.
        lb = find_mailbox(mailboxes(), name=LABEL)
        if not lb:
            return None
        q = method(jmap([["Email/query", {"filter": {"inMailbox": lb["id"]},
                                           "calculateTotal": True}, "0"]]), "Email/query")
        ids = q.get("ids", [])
        return ids[0] if ids else None

    target = poll(bridge_label_target, 180)
    check("Gmail-applied label appears in the bridge", bool(target),
          f"message {target}" if target else "not seen within 180 s")
    stop_if_throttled("STORE -label", *M.store(seq, "-X-GM-LABELS", f'("{LABEL}")'))
    pace()

    # --- 2. archive removes INBOX membership, then undo -------------------
    if not target:
        check("archive removes INBOX membership", False, "no target id (label sync failed)")
        check("archive undone (message back in INBOX)", False, "no target id")
    else:
        r = method(jmap([["Email/set", {"update": {target: {f"mailboxIds/{inbox['id']}": None}}}, "0"]]),
                   "Email/set")
        check("archive accepted by the bridge", not r.get("notUpdated"), json.dumps(r.get("notUpdated", {}))[:160])
        gone = poll(lambda: not inbox_has_msgid(M, gmail_msgid), 90)
        check("archive removes INBOX membership", bool(gone))
        r = method(jmap([["Email/set", {"update": {target: {f"mailboxIds/{inbox['id']}": True}}}, "0"]]),
                   "Email/set")
        check("archive undone accepted by the bridge", not r.get("notUpdated"), json.dumps(r.get("notUpdated", {}))[:160])
        back = poll(lambda: inbox_has_msgid(M, gmail_msgid), 90)
        check("archive undone (message back in INBOX)", bool(back))
    pace()

    # --- 3. compose + send over Gmail SMTP --------------------------------
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
            "identityId": identity_id, "emailId": "#d1"}},
            "onSuccessUpdateEmail": {"#s1": {
                f"mailboxIds/{drafts['id']}": None,
                f"mailboxIds/{sent['id']}": True,
                "keywords/$draft": None,
            }}}, "1"]])
    sub = method(resp, "EmailSubmission/set")
    check("submission accepted", bool(sub.get("created", {}).get("s1")),
          json.dumps(sub.get("notCreated", {}))[:200])

    # The sent message's Message-ID, so delivery can be proven on All Mail
    # (where both copies land at once) instead of the lagging INBOX index.
    send_id = method(resp, "Email/set").get("created", {}).get("d1", {}).get("id", "")
    send_mid = ""
    if send_id:
        mid = method(jmap([["Email/get", {"ids": [send_id], "properties": ["messageId"]}, "0"]]),
                     "Email/get").get("list", [{}])[0].get("messageId") or []
        send_mid = mid[0] if mid else ""

    def delivered():
        # The received copy is a distinct All Mail message that is neither
        # the filed Sent copy (\Sent) nor a still-unsent draft (\Drafts);
        # a normal delivery also carries \Inbox. All Mail is used because
        # its index is immediate, unlike INBOX's, which can lag minutes.
        M.select('"[Gmail]/All Mail"', readonly=True)
        seqs = []
        if send_mid:
            typ, data = M.search(None, '(HEADER Message-ID "%s")' % send_mid)
            seqs = data[0].split() if data and data[0] else []
        if not seqs:  # header search not honoured: the unique subject will do
            typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
            seqs = data[0].split() if data and data[0] else []
        out = []
        for s in seqs:
            if has_label(M, s, "\\Inbox"):
                out.append(s)
            elif not has_label(M, s, "\\Sent") and not has_label(M, s, "\\Drafts"):
                out.append(s)
        return out or None

    got = poll(delivered, 240)
    check("sent message delivered", bool(got), f"{len(got or [])} copy(ies)")
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

    finish(M, t_start)


def finish(M, t_start):
    # Best-effort cleanup, carried out inside All Mail (a message's UIDs are
    # per-folder, so acting on All Mail reaches every membership at once).
    try:
        M.select('"[Gmail]/All Mail"')
        typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
        uids = [fetch_uid(M, s) for s in data[0].split()] if data and data[0] else []
        if uids:
            uidset = ",".join(uids)
            M.uid("STORE", uidset, "+X-GM-LABELS", "(\\Trash)")
            for lbl in (LABEL, "\\Inbox"):
                try:
                    M.uid("STORE", uidset, "-X-GM-LABELS", f"({lbl})")
                except imaplib.IMAP4.error:
                    pass
            M.select('"[Gmail]/Bin"')
            typ, data = M.search(None, '(HEADER Subject "%s")' % SUBJECT.replace('"', ""))
            buids = [fetch_uid(M, s) for s in data[0].split()] if data and data[0] else []
            if buids:
                buset = ",".join(buids)
                M.uid("STORE", buset, "+FLAGS", "(\\Deleted)")
                # UID EXPUNGE needs a uid set; a bare EXPUNGE also removes
                # any other \Deleted message another client flagged.
                M.uid("EXPUNGE", buset)
        try:
            M.delete(f'"{LABEL}"')
        except imaplib.IMAP4.error:
            pass
        M.logout()
    except Exception as e:  # cleanup must never mask the result
        print("cleanup note:", type(e).__name__, str(e)[:120])

    print(f"---- {time.time()-t_start:.0f}s, {len(failures)} failure(s)")
    if failures:
        print("FAILED:", ", ".join(failures))
        sys.exit(1)
    print("M4 GATE: all checks passed")


if __name__ == "__main__":
    main()
