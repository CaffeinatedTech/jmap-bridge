// Package fixturesmtp is the in-process SMTP submission server the
// send-path tests and the M3 gate deliver to (AGENTS.md testing rules:
// protocol behaviour ships with an in-process fixture — no real-network
// unit tests, ever). It speaks enough RFC 5321 to be a submission sink:
// EHLO/AUTH/MAIL/RCPT/DATA, dot-unstuffing the message back into the
// bytes that were sent, and the refusals a real server makes so the
// failure paths are testable too.
//
// Nothing here pretends to be a mail transfer agent: it stores what it
// receives and never forwards it.
package fixturesmtp

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
)

// TB is everything a fixture needs from its host: a way to register
// cleanup and a way to fail. [testing.TB] satisfies it, and so does the
// small adapter a standalone sink program uses — the fixture never
// imports the testing package itself.
type TB interface {
	Cleanup(func())
	Fatalf(format string, args ...any)
	Helper()
}

// Options configures a fixture submission server.
type Options struct {
	// Username and Password are the AUTH PLAIN credentials. With both
	// empty the server advertises no AUTH at all, like a submission
	// service on a trusted network.
	Username string
	Password string
	// FailAuth refuses AUTH with 535, the wrong-password answer.
	FailAuth bool
	// RejectMail, when set, refuses MAIL FROM with this reply
	// ("550 5.7.1 sender not permitted").
	RejectMail string
	// RejectRCPT, when set, refuses every RCPT TO with this reply.
	RejectRCPT string
	// OnMessage, when set, is called once a submission has been
	// accepted — outside the store lock, on the connection's goroutine.
	// A harness that has to behave like an MTA (deliver the message
	// into a local mailbox so a client's live view can see it) hands
	// the work here; the fixture itself only stores.
	OnMessage func(Message)
}

// Message is one accepted submission: the envelope and the bytes as the
// server received them (dot-unstuffed, CRLF intact).
type Message struct {
	From       string
	Recipients []string
	Data       []byte
}

// Server is a running fixture SMTP server.
type Server struct {
	addr string
	ln   net.Listener
	opts Options

	mu   sync.Mutex
	msgs []Message
}

// Start brings a fixture submission server up on a loopback ephemeral
// port and stops it on test cleanup. host returns the address to dial;
// it is 127.0.0.1, which is what makes AUTH PLAIN legal without TLS
// (net/smtp only sends credentials over TLS or to localhost).
func Start(t TB, opts Options) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fixturesmtp: listen: %v", err)
	}
	s := &Server{addr: ln.Addr().String(), ln: ln, opts: opts}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Addr returns the "host:port" to configure as the submission server.
func (s *Server) Addr() string { return s.addr }

// Host and Port split Addr for the config shape.
func (s *Server) Host() string {
	host, _, err := net.SplitHostPort(s.addr)
	if err != nil {
		return s.addr
	}
	return host
}

// Port returns the fixture's TCP port.
func (s *Server) Port() int {
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return 0
	}
	var n int
	for _, c := range port {
		n = n*10 + int(c-'0')
	}
	return n
}

// Messages returns the submissions accepted so far, oldest first.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.msgs))
	copy(out, s.msgs)
	return out
}

// Reset forgets every accepted submission (idempotent test setup).
func (s *Server) Reset() {
	s.mu.Lock()
	s.msgs = nil
	s.mu.Unlock()
}

// Close stops the listener; existing connections finish their command.
func (s *Server) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	send("220 fixturesmtp ESMTP ready")
	var (
		from   string
		rcpts  []string
		authed bool
	)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg := splitVerb(line)
		switch verb {
		case "EHLO":
			_, _ = w.WriteString("250-fixturesmtp\r\n250-8BITMIME\r\n250-SIZE 67108864\r\n")
			if s.opts.Username != "" || s.opts.Password != "" {
				_, _ = w.WriteString("250 AUTH PLAIN\r\n")
			} else {
				_, _ = w.WriteString("250 HELP\r\n")
			}
			_ = w.Flush()
		case "HELO":
			send("250 fixturesmtp")
		case "AUTH":
			reply := s.auth(arg)
			send(reply)
			authed = strings.HasPrefix(reply, "235")
		case "MAIL":
			if s.opts.RejectMail != "" {
				send(s.opts.RejectMail)
				continue
			}
			if authed || (s.opts.Username == "" && s.opts.Password == "") {
				from = parsePath(arg)
				send("250 OK")
			} else {
				send("530 5.7.0 Authentication required")
			}
		case "RCPT":
			if s.opts.RejectRCPT != "" {
				send(s.opts.RejectRCPT)
				continue
			}
			if from == "" {
				send("503 5.5.1 Need MAIL command first")
				continue
			}
			rcpts = append(rcpts, parsePath(arg))
			send("250 OK")
		case "DATA":
			if from == "" || len(rcpts) == 0 {
				send("503 5.5.1 Need MAIL and RCPT first")
				continue
			}
			send("354 End data with <CR><LF>.<CR><LF>")
			data, ok := readData(r)
			if !ok {
				return
			}
			msg := Message{From: from, Recipients: append([]string(nil), rcpts...), Data: data}
			s.mu.Lock()
			s.msgs = append(s.msgs, msg)
			s.mu.Unlock()
			from, rcpts = "", nil
			send("250 OK: queued")
			if s.opts.OnMessage != nil {
				s.opts.OnMessage(msg)
			}
		case "RSET":
			from, rcpts = "", nil
			send("250 OK")
		case "NOOP":
			send("250 OK")
		case "QUIT":
			send("221 bye")
			return
		case "VRFY":
			send("252 cannot verify")
		default:
			send("500 5.5.2 Command not implemented")
		}
	}
}

// auth answers AUTH PLAIN with an initial response in arg.
func (s *Server) auth(arg string) string {
	fields := strings.Fields(arg)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "PLAIN") {
		return "504 5.5.4 Unrecognized authentication type"
	}
	if s.opts.FailAuth {
		return "535 5.7.8 Authentication credentials invalid"
	}
	if s.opts.Username == "" && s.opts.Password == "" {
		return "535 5.7.8 Authentication unavailable"
	}
	if len(fields) == 1 {
		return "334 " // no initial response: ask for it
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "535 5.7.8 Authentication credentials invalid"
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 || parts[1] != s.opts.Username || parts[2] != s.opts.Password {
		return "535 5.7.8 Authentication credentials invalid"
	}
	return "235 2.7.0 Authentication successful"
}

// splitVerb splits "MAIL FROM:<a@b>" into its verb and argument.
func splitVerb(line string) (verb, arg string) {
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return strings.ToUpper(line[:i]), line[i+1:]
	}
	return strings.ToUpper(line), ""
}

// parsePath extracts the address from a command argument such as
// "FROM:<a@b.c>" or "TO:<a@b.c> SIZE=123".
func parsePath(arg string) string {
	start := strings.IndexByte(arg, '<')
	end := strings.IndexByte(arg, '>')
	if start >= 0 && end > start {
		return arg[start+1 : end]
	}
	return strings.TrimSpace(arg)
}

// readData consumes the DATA body until the RFC 5321 terminator,
// undoing dot-stuffing so the message is what the client wrote. Lines
// arrive CRLF-terminated (net/smtp's DotWriter guarantees it), so the
// bytes out are byte-identical to a message that ended in CRLF.
func readData(r *bufio.Reader) ([]byte, bool) {
	var out []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, false
		}
		body := strings.TrimRight(line, "\r\n")
		if body == "." {
			return out, true
		}
		if strings.HasPrefix(body, "..") {
			body = body[1:]
		}
		out = append(out, body...)
		out = append(out, "\r\n"...)
	}
}

// String renders a message for a failure message.
func (m Message) String() string {
	return fmt.Sprintf("MAIL FROM:<%s> RCPT TO:%v (%d bytes)", m.From, m.Recipients, len(m.Data))
}
