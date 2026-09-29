package submit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixturesmtp"
)

// These tests pin the SMTP half of PLAN §7.2 against the in-process
// fixture (AGENTS.md testing rules: no real-network unit tests).

// testConfig points a Config at a fixture on loopback. tls is "none",
// which is also what makes AUTH PLAIN legal there (net/smtp only sends
// credentials over TLS or to localhost).
func testConfig(sink *fixturesmtp.Server) Config {
	return Config{
		Host:     sink.Host(),
		Port:     sink.Port(),
		TLS:      "none",
		Auth:     "password",
		Username: "submitter",
		Password: "submit-pass",
	}
}

// messageFixture is a message with the shapes that break naive SMTP
// clients: CRLF line endings, a line that begins with a dot (which the
// client must stuff and the server must unstuff), and non-ASCII bytes.
func messageFixture() []byte {
	return []byte("From: me@example.test\r\n" +
		"To: you@example.test\r\n" +
		"Subject: round trip\r\n" +
		"\r\n" +
		"first line\r\n" +
		".this line starts with a dot\r\n" +
		"..and this one with two\r\n" +
		"last line without trailing CRLF") // no final CRLF: the client adds it
}

func waitMessages(t *testing.T, sink *fixturesmtp.Server, want int) []fixturesmtp.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := sink.Messages(); len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sink received %d messages, want %d", len(sink.Messages()), want)
	return nil
}

func TestSendDeliversBytesExactly(t *testing.T) {
	sink := fixturesmtp.Start(t, fixturesmtp.Options{
		Username: "submitter", Password: "submit-pass",
	})
	msg := messageFixture()

	err := Send(context.Background(), testConfig(sink), Envelope{
		From:       "me@example.test",
		Recipients: []string{"you@example.test", "bcc@example.test"},
	}, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := waitMessages(t, sink, 1)[0]
	if got.From != "me@example.test" {
		t.Errorf("MAIL FROM = %q", got.From)
	}
	if len(got.Recipients) != 2 || got.Recipients[0] != "you@example.test" || got.Recipients[1] != "bcc@example.test" {
		t.Errorf("RCPT TO = %v, want both recipients in order", got.Recipients)
	}
	// Dot-unstuffing must restore the message byte for byte: every line
	// arrives CRLF-terminated, so the comparison holds only if the
	// client stuffed and the fixture unstuffed correctly.
	if string(got.Data) != string(msg)+"\r\n" {
		t.Errorf("delivered bytes differ:\n got %q\nwant %q", got.Data, string(msg)+"\r\n")
	}
}

func TestSendReportsServerRefusals(t *testing.T) {
	t.Run("recipient rejected", func(t *testing.T) {
		sink := fixturesmtp.Start(t, fixturesmtp.Options{
			Username: "submitter", Password: "submit-pass",
			RejectRCPT: "550 5.1.1 no such recipient",
		})
		err := Send(context.Background(), testConfig(sink), Envelope{
			From: "me@example.test", Recipients: []string{"nobody@example.test"},
		}, messageFixture())
		var rejected *RejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("err = %v, want *RejectedError", err)
		}
		if !strings.Contains(rejected.Reply, "550") || !strings.Contains(rejected.Reply, "no such recipient") {
			t.Errorf("reply = %q, want the server's own words (PLAN §7.2)", rejected.Reply)
		}
		if n := len(sink.Messages()); n != 0 {
			t.Errorf("sink holds %d messages, want none for a refused submission", n)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		sink := fixturesmtp.Start(t, fixturesmtp.Options{
			Username: "submitter", Password: "other-pass",
		})
		err := Send(context.Background(), testConfig(sink), Envelope{
			From: "me@example.test", Recipients: []string{"you@example.test"},
		}, messageFixture())
		var rejected *RejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("err = %v, want *RejectedError", err)
		}
		if !strings.Contains(rejected.Reply, "535") {
			t.Errorf("reply = %q, want the 535 the server sent", rejected.Reply)
		}
	})

	t.Run("server offers no AUTH", func(t *testing.T) {
		sink := fixturesmtp.Start(t, fixturesmtp.Options{}) // no credentials configured
		err := Send(context.Background(), testConfig(sink), Envelope{
			From: "me@example.test", Recipients: []string{"you@example.test"},
		}, messageFixture())
		if err == nil || !strings.Contains(err.Error(), "AUTH") {
			t.Fatalf("err = %v, want a sentence about AUTH", err)
		}
	})
}

func TestSendRefusesUnsupportedConfigurations(t *testing.T) {
	sink := fixturesmtp.Start(t, fixturesmtp.Options{Username: "submitter", Password: "submit-pass"})
	cfg := testConfig(sink)

	if err := Send(context.Background(), Config{Host: cfg.Host, Port: cfg.Port, Auth: "oauth2"},
		Envelope{From: "me@example.test", Recipients: []string{"you@example.test"}}, messageFixture()); err == nil ||
		!strings.Contains(err.Error(), "oauth2") {
		t.Errorf("oauth2 without a token provider = %v, want a refusal naming oauth2 (FR-A.10)", err)
	}
	if err := Send(context.Background(), cfg, Envelope{From: "", Recipients: []string{"a@b.c"}}, nil); err == nil {
		t.Error("empty envelope sender accepted")
	}
	if err := Send(context.Background(), cfg, Envelope{From: "me@example.test"}, nil); err == nil {
		t.Error("empty recipient list accepted")
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("sink holds %d messages; a refused configuration must not send", n)
	}
}

func TestSendHonoursContext(t *testing.T) {
	sink := fixturesmtp.Start(t, fixturesmtp.Options{Username: "submitter", Password: "submit-pass"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Send(ctx, testConfig(sink), Envelope{
		From: "me@example.test", Recipients: []string{"you@example.test"},
	}, messageFixture()); err == nil {
		t.Fatal("Send with a cancelled context succeeded")
	}
}

// XOAUTH2 submission (FR-A.7): the bearer token rides in the SASL
// initial response, a rejected token is retried exactly once with a
// forced refresh, and the message lands in the sink on success only.
func TestSendXOAUTH2(t *testing.T) {
	sink := fixturesmtp.Start(t, fixturesmtp.Options{Username: "me@example.test", OAuthToken: "good-token"})
	tokens := []string{"stale-token", "good-token"}
	var forces []bool
	cfg := Config{
		Host: sink.Host(), Port: sink.Port(), TLS: "none", Auth: "oauth2",
		Username: "me@example.test",
		Token: func(_ context.Context, force bool) (string, error) {
			forces = append(forces, force)
			return tokens[len(forces)-1], nil
		},
	}
	if err := Send(context.Background(), cfg, Envelope{
		From: "me@example.test", Recipients: []string{"you@example.test"},
	}, messageFixture()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(forces) != 2 || !forces[1] {
		t.Fatalf("token provider calls = %v, want initial + forced retry", forces)
	}
	waitMessages(t, sink, 1)
}

func TestSendXOAUTH2SuccessWithoutRetry(t *testing.T) {
	sink := fixturesmtp.Start(t, fixturesmtp.Options{Username: "me@example.test", OAuthToken: "good-token"})
	calls := 0
	cfg := Config{
		Host: sink.Host(), Port: sink.Port(), TLS: "none", Auth: "oauth2",
		Username: "me@example.test",
		Token: func(_ context.Context, _ bool) (string, error) {
			calls++
			return "good-token", nil
		},
	}
	if err := Send(context.Background(), cfg, Envelope{
		From: "me@example.test", Recipients: []string{"you@example.test"},
	}, messageFixture()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if calls != 1 {
		t.Fatalf("token provider called %d times, want 1", calls)
	}
}

func TestSendXOAUTH2RejectedAfterRetry(t *testing.T) {
	sink := fixturesmtp.Start(t, fixturesmtp.Options{Username: "me@example.test", OAuthToken: "good-token", FailAuth: true})
	cfg := Config{
		Host: sink.Host(), Port: sink.Port(), TLS: "none", Auth: "oauth2",
		Username: "me@example.test",
		Token: func(_ context.Context, _ bool) (string, error) {
			return "dead-token", nil
		},
	}
	err := Send(context.Background(), cfg, Envelope{
		From: "me@example.test", Recipients: []string{"you@example.test"},
	}, messageFixture())
	if err == nil {
		t.Fatal("send succeeded with rejected credentials")
	}
	if n := len(sink.Messages()); n != 0 {
		t.Errorf("sink holds %d messages after a rejected send", n)
	}
}
