package imapdrv

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// The XOAUTH2 dial path: the initial response carries user and bearer
// token; a rejected token is retried exactly once with a forced refresh
// (FR-A.7), and a second rejection surfaces as a dial failure.
func TestDialXOAUTH2(t *testing.T) {
	tokens := []string{"tok-1", "tok-2"}
	var calls []bool
	provider := func(_ context.Context, force bool) (string, error) {
		calls = append(calls, force)
		return tokens[len(calls)-1], nil
	}
	f := startFakeIMAP(t, "CAPABILITY IMAP4rev1 AUTH=XOAUTH2 SASL-IR", func(tag, line string) []string {
		if strings.HasPrefix(line, "AUTH ") || strings.Contains(line, " AUTHENTICATE") {
			b64 := line[strings.LastIndex(line, " ")+1:]
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Errorf("AUTHENTICATE initial response is not base64: %v", err)
				return []string{tag + " NO bad"}
			}
			want := "user=tester\x01auth=Bearer " + tokens[len(calls)-1] + "\x01\x01"
			if string(raw) != want {
				t.Errorf("XOAUTH2 response = %q, want %q", raw, want)
			}
			if len(calls) == 1 {
				return []string{tag + " NO [AUTHENTICATIONFAILED] expired"}
			}
			return []string{tag + " OK authenticated"}
		}
		return nil
	})
	cfg := Config{Username: "tester", Token: provider, Logger: testLogger()}
	c := dialFake(t, f, cfg)
	if len(calls) != 2 {
		t.Fatalf("token provider called %d times, want 2 (initial + forced retry)", len(calls))
	}
	if !calls[1] {
		t.Fatal("the retry did not force a refresh")
	}
	_ = c
}

func TestDialXOAUTH2NoFallbackToPassword(t *testing.T) {
	// FR-A.10: with a token provider set there is no LOGIN fallback — a
	// second rejection fails the dial.
	calls := 0
	provider := func(_ context.Context, _ bool) (string, error) {
		calls++
		return "dead-token", nil
	}
	f := startFakeIMAP(t, "CAPABILITY IMAP4rev1 AUTH=XOAUTH2 SASL-IR", func(tag, line string) []string {
		if strings.Contains(line, "AUTHENTICATE") {
			return []string{tag + " NO [AUTHENTICATIONFAILED] rejected"}
		}
		if strings.Contains(line, "LOGIN") {
			t.Error("password LOGIN used despite a token provider")
			return []string{tag + " NO"}
		}
		return nil
	})
	_, err := func() (*Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return Dial(ctx, Config{Host: f.Host(), Port: f.Port(), Username: "tester", Password: "should-not-be-used", Token: provider})
	}()
	if err == nil {
		t.Fatal("dial succeeded despite two rejected tokens")
	}
	if calls != 2 {
		t.Fatalf("token provider called %d times, want exactly 2", calls)
	}
}

// The Gmail header fetch carries X-GM-LABELS and X-GM-THRID; the values
// arrive as raw wire bytes and must decode to labels (flag-form system
// labels, quoted user labels) and the 64-bit thread id (FR-S.10).
func TestFetchGmLabelsAndThrid(t *testing.T) {
	f := startFakeIMAP(t, "CAPABILITY IMAP4rev1 AUTH=PLAIN", func(tag, line string) []string {
		switch {
		case strings.Contains(line, "CAPABILITY"):
			return []string{"* CAPABILITY IMAP4rev1 X-GM-EXT-1"}
		case strings.Contains(line, " SELECT ") || strings.Contains(line, " EXAMINE "):
			return []string{
				"* 2 EXISTS",
				"* OK [UIDVALIDITY 42] uids",
				"* OK [UIDNEXT 3] next",
			}
		case strings.Contains(line, "UID FETCH"):
			return []string{
				`* 1 FETCH (UID 1 FLAGS (\Seen) MODSEQ (77) INTERNALDATE "01-Jan-2026 10:00:00 +0000" ` +
					`RFC822.SIZE 100 ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL) ` +
					`BODYSTRUCTURE ("TEXT" "PLAIN" NIL NIL NIL "7BIT" 10 1) ` +
					`X-GM-LABELS (\Inbox "my receipts" work) X-GM-THRID 1276603744173617373)`,
				`* 2 FETCH (UID 2 FLAGS () MODSEQ (78) INTERNALDATE "01-Jan-2026 10:00:00 +0000" ` +
					`RFC822.SIZE 100 ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL) ` +
					`BODYSTRUCTURE ("TEXT" "PLAIN" NIL NIL NIL "7BIT" 10 1) X-GM-LABELS NIL X-GM-THRID 5)`,
			}
		}
		return nil
	})
	c := dialFake(t, f, Config{Username: "tester", Password: "pw"})
	if !c.GmailExt() {
		t.Fatal("X-GM-EXT-1 not detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Examine(ctx, "INBOX", nil); err != nil {
		t.Fatalf("examine: %v", err)
	}
	msgs, err := c.FetchHeaders(ctx, []uint32{1, 2})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	got := strings.Join(msgs[0].Labels, "|")
	if got != `\Inbox|my receipts|work` {
		t.Errorf("labels = %q", got)
	}
	if msgs[0].Thrid != 1276603744173617373 {
		t.Errorf("thrid = %d", msgs[0].Thrid)
	}
	if msgs[1].Labels != nil {
		t.Errorf("NIL labels decoded as %v", msgs[1].Labels)
	}
	if msgs[1].Thrid != 5 {
		t.Errorf("second thrid = %d", msgs[1].Thrid)
	}
}

// The label store writes the exact Gmail wire form, .SILENT included
// (FR-S.10), on the folder the caller selected.
func TestStoreGmLabelsWire(t *testing.T) {
	f := startFakeIMAP(t, "CAPABILITY IMAP4rev1 AUTH=PLAIN", func(tag, line string) []string {
		switch {
		case strings.Contains(line, "CAPABILITY"):
			return []string{"* CAPABILITY IMAP4rev1 UIDPLUS X-GM-EXT-1"}
		case strings.Contains(line, " SELECT "):
			return []string{
				"* 1 EXISTS",
				"* OK [UIDVALIDITY 42] uids",
				"* OK [UIDNEXT 9] next",
				tag + " OK [READ-WRITE] selected",
			}
		case strings.Contains(line, "UNSELECT"):
			return []string{tag + " OK bye"}
		}
		return nil
	})
	c := dialFake(t, f, Config{Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.StoreGmLabels(ctx, "INBOX", []uint32{7},
		[]string{`\Sent`, "my receipts"}, []string{`\Inbox`}); err != nil {
		t.Fatalf("store: %v", err)
	}
	got := f.LineMatching("UID STORE")
	want := `UID STORE 7 +X-GM-LABELS.SILENT (\Sent "my receipts")`
	if !strings.Contains(got, want) {
		t.Errorf("add line = %q, want it to contain %q", got, want)
	}
	got = f.LineMatching("-X-GM-LABELS")
	want = `UID STORE 7 -X-GM-LABELS.SILENT (\Inbox)`
	if !strings.Contains(got, want) {
		t.Errorf("remove line = %q, want it to contain %q", got, want)
	}
}

// Discovery maps the \All attribute to the archive role (FR-M.18's
// archive semantics) and marks it All Mail.
func TestListFoldersAllMail(t *testing.T) {
	f := startFakeIMAP(t, "CAPABILITY IMAP4rev1 AUTH=PLAIN", func(tag, line string) []string {
		if strings.Contains(line, "CAPABILITY") {
			return []string{"* CAPABILITY IMAP4rev1 X-GM-EXT-1 LIST-EXTENDED SPECIAL-USE"}
		}
		if strings.Contains(line, " LIST ") {
			return []string{
				`* LIST (\HasNoChildren \All) "/" "[Gmail]/All Mail"`,
				`* LIST (\HasNoChildren \Sent) "/" "[Gmail]/Sent Mail"`,
				`* LIST (\HasNoChildren) "/" INBOX`,
			}
		}
		if strings.Contains(line, "STATUS") {
			arg := line[strings.Index(line, "STATUS")+len("STATUS "):]
			return []string{
				"* STATUS " + arg + " (MESSAGES 1 UIDNEXT 2 UIDVALIDITY 7 UNSEEN 0 HIGHESTMODSEQ 9)",
			}
		}
		return nil
	})
	c := dialFake(t, f, Config{Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	folders, err := c.ListFolders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]Folder{}
	for _, fl := range folders {
		byName[fl.Name] = fl
	}
	all := byName["[Gmail]/All Mail"]
	if all.Role != "archive" || !all.AllMail {
		t.Errorf("All Mail = role %q allMail %v", all.Role, all.AllMail)
	}
	if byName["INBOX"].Role != "inbox" {
		t.Errorf("INBOX role = %q", byName["INBOX"].Role)
	}
	if byName["[Gmail]/Sent Mail"].Role != "sent" {
		t.Errorf("Sent Mail role = %q", byName["[Gmail]/Sent Mail"].Role)
	}
}
