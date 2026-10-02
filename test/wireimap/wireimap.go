// Package wireimap is a raw wire-level IMAP server for tests that must
// assert exact protocol behaviour the fixture server cannot express —
// Gmail's X-GM-* extensions and XOAUTH2 exchanges. It answers one line
// per handler call; a handler that scripts no tagged reply gets a bare
// tagged OK appended, so exchanges keep moving.
//
// It accepts any number of connections, each scripted by the same
// handler: an engine opens a separate session for sync, writes and
// hydration (FR-X.6), so a one-connection fake would deadlock the
// second dialer.
//
// Test-only: never import from production code.
package wireimap

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Server is one scripted connection's listener.
type Server struct {
	t      testing.TB
	ln     net.Listener
	mu     sync.Mutex
	lines  []string
	handle func(tag, line string) []string
}

// Start serves one scripted connection on a loopback ephemeral port and
// closes everything on test cleanup.
func Start(t testing.TB, caps string, handle func(tag, line string) []string) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("wireimap: listen: %v", err)
	}
	s := &Server{t: t, ln: ln, handle: handle}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, caps)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// serve scripts one connection until it closes.
func (s *Server) serve(conn net.Conn, caps string) {
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("* OK [" + caps + "] ready\r\n"))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag := "*"
		if fields := strings.Fields(line); len(fields) > 0 {
			tag = fields[0]
		}
		s.mu.Lock()
		s.lines = append(s.lines, line)
		s.mu.Unlock()
		replies := s.handle(tag, line)
		for _, reply := range replies {
			if _, err := conn.Write([]byte(reply + "\r\n")); err != nil {
				return
			}
		}
		// A handler that scripted no tagged reply for this command gets
		// a bare success appended.
		tagged := false
		for _, reply := range replies {
			if strings.HasPrefix(reply, tag+" ") {
				tagged = true
				break
			}
		}
		if !tagged {
			if _, err := conn.Write([]byte(tag + " OK done\r\n")); err != nil {
				return
			}
		}
	}
}

// Close tears the listener down early.
func (s *Server) Close() { _ = s.ln.Close() }

// Host and Port return the loopback address to dial.
func (s *Server) Host() string {
	host, _, _ := net.SplitHostPort(s.ln.Addr().String())
	return host
}

func (s *Server) Port() int {
	_, portStr, _ := net.SplitHostPort(s.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port
}

// LineMatching returns the first command line containing substr.
func (s *Server) LineMatching(substr string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		if strings.Contains(l, substr) {
			return l
		}
	}
	return ""
}

// CountMatching counts command lines containing substr.
func (s *Server) CountMatching(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// Lines returns every command line the client sent.
func (s *Server) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}
