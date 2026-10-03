// Command gmailfixture runs the in-process Gmail API fixture as a
// standalone HTTP server for the M10 cross-client gate: jmap-tui browses
// a bridge whose backend is this fixture (GMAIL_API_PLAN §12). It seeds a
// deterministic small mailbox and prints the URL and bearer token the
// bridge config needs, then serves until interrupted.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

func main() {
	fx := fixturegmail.StartServer(fixturegmail.Options{User: "fixture@example.test", Token: "fixture-token"})
	defer fx.Close()

	fx.SeedLabel(fixturegmail.LabelInbox, "INBOX", "system")
	fx.SeedLabel(fixturegmail.LabelSent, "SENT", "system")
	fx.SeedLabel(fixturegmail.LabelDraft, "DRAFT", "system")
	fx.SeedLabel(fixturegmail.LabelTrash, "TRASH", "system")
	fx.SeedLabel(fixturegmail.LabelSpam, "SPAM", "system")
	fx.SeedLabel("Label_work", "Work", "user")

	now := time.Now().UnixMilli()
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "fx1", ThreadID: "fxt1", InternalDate: now,
		LabelIDs: []string{fixturegmail.LabelInbox, fixturegmail.LabelUnread, "Label_work"},
		Snippet:  "Fixture message one",
		Headers: []fixturegmail.Header{
			{Name: "From", Value: "Alice <alice@example.test>"},
			{Name: "To", Value: "fixture@example.test"},
			{Name: "Subject", Value: "Fixture one"},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:04:05 -0700"},
			{Name: "Message-ID", Value: "<fx1@fixture.test>"},
		},
		Raw: []byte("From: alice@example.test\r\nSubject: Fixture one\r\nMessage-ID: <fx1@fixture.test>\r\n\r\none\r\n"),
	})
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "fx2", ThreadID: "fxt2", InternalDate: now - 60000,
		LabelIDs: []string{fixturegmail.LabelInbox, fixturegmail.LabelStarred},
		Snippet:  "Fixture message two",
		Headers: []fixturegmail.Header{
			{Name: "From", Value: "Bob <bob@example.test>"},
			{Name: "To", Value: "fixture@example.test"},
			{Name: "Subject", Value: "Fixture two"},
			{Name: "Date", Value: "Mon, 02 Jan 2026 14:04:05 -0700"},
			{Name: "Message-ID", Value: "<fx2@fixture.test>"},
		},
		Raw: []byte("From: bob@example.test\r\nSubject: Fixture two\r\nMessage-ID: <fx2@fixture.test>\r\n\r\ntwo\r\n"),
	})

	fmt.Printf("URL=%s\nTOKEN=%s\n", fx.URL(), fx.Token())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
