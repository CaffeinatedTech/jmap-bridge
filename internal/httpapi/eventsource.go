package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxPingSeconds bounds the client's ping interval. A value large enough
// to overflow time.Duration would panic time.NewTicker, so the server
// clamps it; pinging more often is always RFC-compliant.
const maxPingSeconds = 3600

// handleEventSource serves GET /{account}/eventsource/ — RFC 8620 §7.3
// (FR-J.8). The session advertises a level-1 URI template; the client
// expands types/closeafter/ping into the query string and this handler
// honours them: "state" closes the stream after one state event, "no"
// persists, and a positive ping is honoured exactly (any clamp of ours
// would have to stay within the RFC's bounds, and there is no reason
// to move off what the client asked for).
func (s *Server) handleEventSource(w http.ResponseWriter, r *http.Request) {
	acct := s.authorize(w, r, r.PathValue("account"))
	if acct == nil {
		if s.cfg.Auth.Mode == "none" {
			http.NotFound(w, r)
		}
		return
	}
	// Every stream holds a goroutine and a hub subscription, so cap them
	// per account and in total (A4). Released when the handler returns.
	if !s.acquireSSE(acct.ID) {
		s.tooManyRequests(w, "maxEventSourceConnections")
		return
	}
	defer s.releaseSSE(acct.ID)

	q := r.URL.Query()
	types := q.Get("types")
	if types != "" && types != "*" && !validTypeList(types) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"type": "urn:ietf:params:jmap:error:invalidArguments", "status": 400,
			"detail": "types must be * or a comma-separated list of type names",
		}, nil)
		return
	}
	closeAfter := q.Get("closeafter")
	switch closeAfter {
	case "", "no":
		closeAfter = "no"
	case "state":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"type": "urn:ietf:params:jmap:error:invalidArguments", "status": 400,
			"detail": "closeafter must be \"state\" or \"no\"",
		}, nil)
		return
	}
	ping := 0
	if raw := q.Get("ping"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"type": "urn:ietf:params:jmap:error:invalidArguments", "status": 400,
				"detail": "ping must be a non-negative integer",
			}, nil)
			return
		}
		// A very large value overflows time.Duration and makes the
		// ticker panic; cap it. Pinging more often than asked still
		// satisfies RFC 8620 §7.3's "at least every ping seconds".
		if v > maxPingSeconds {
			v = maxPingSeconds
		}
		ping = v
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// X-Accel-Buffering asks nginx not to buffer; README §Deployment
	// documents the proxy requirements (FR-D.8).
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe before the snapshot so a change during setup is either
	// already in the snapshot or arrives as a signal — never neither.
	signal, unsubscribe := s.hub.Subscribe(acct.ID)
	defer unsubscribe()

	last, err := s.store.States(r.Context(), acct.ID)
	if err != nil {
		return
	}
	// A reconnecting client sends Last-Event-ID; if it is not our
	// current id the client missed changes while away — push current
	// state immediately instead of waiting for the next change (§7.3
	// SHOULD).
	if prev := r.Header.Get("Last-Event-ID"); prev != "" && prev != stateID(last) {
		if !writeEvent(w, flusher, "state", stateID(last), stateEvent(acct.ID, last, types)) {
			return
		}
		if closeAfter == "state" {
			return
		}
	}

	var pingC <-chan time.Time
	var ticker *time.Ticker
	if ping > 0 {
		ticker = time.NewTicker(time.Duration(ping) * time.Second)
		defer ticker.Stop()
		pingC = ticker.C
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-signal:
			states, err := s.store.States(r.Context(), acct.ID)
			if err != nil {
				return
			}
			changed := diffStates(last, states, types)
			if len(changed) == 0 {
				continue // duplicate signal: the client already knows
			}
			last = states
			if !writeEvent(w, flusher, "state", stateID(last), stateEvent(acct.ID, changed, types)) {
				return
			}
			if closeAfter == "state" {
				return
			}
		case <-pingC:
			// Pings carry no id and do not advance the id stream (§7.3).
			if !writeEvent(w, flusher, "ping", "", json.RawMessage(fmt.Sprintf(`{"interval":%d}`, ping))) {
				return
			}
		}
	}
}

func validTypeList(raw string) bool {
	for _, t := range strings.Split(raw, ",") {
		if strings.TrimSpace(t) == "" {
			return false
		}
	}
	return true
}

// stateEvent builds the StateChange payload (RFC 8620 §7.1): the types
// requested by the connection, filtered to the ones that moved.
func stateEvent(accountID string, states map[string]string, types string) json.RawMessage {
	changed := map[string]string{}
	for typ, st := range states {
		if types != "" && types != "*" && !typeRequested(types, typ) {
			continue
		}
		changed[typ] = st
	}
	raw, err := json.Marshal(map[string]any{
		"@type":   "StateChange",
		"changed": map[string]any{accountID: changed},
	})
	if err != nil {
		return json.RawMessage(`{"@type":"StateChange"}`)
	}
	return raw
}

func typeRequested(types, typ string) bool {
	for _, t := range strings.Split(types, ",") {
		if strings.TrimSpace(t) == typ {
			return true
		}
	}
	return false
}

// diffStates returns the entries of next that differ from last.
func diffStates(last, next map[string]string, types string) map[string]string {
	out := map[string]string{}
	for typ, st := range next {
		if types != "" && types != "*" && !typeRequested(types, typ) {
			continue
		}
		if last[typ] != st {
			out[typ] = st
		}
	}
	return out
}

// stateID is the id line: the full state snapshot, so a reconnecting
// client can be compared against it byte-for-byte (§7.3).
func stateID(states map[string]string) string {
	parts := make([]string, 0, len(states))
	for _, typ := range []string{"Email", "Mailbox", "Thread", "AddressBook", "ContactCard"} {
		if st, ok := states[typ]; ok {
			parts = append(parts, typ+":"+st)
		}
	}
	return strings.Join(parts, ";")
}

// writeEvent writes one named SSE event and flushes. id is emitted
// only for state events: pings must not advance the id stream (§7.3).
func writeEvent(w http.ResponseWriter, flusher http.Flusher, name, id string, data json.RawMessage) bool {
	var err error
	if name == "state" {
		_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, name, data)
	} else {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	}
	if err != nil {
		return false
	}
	flusher.Flush()
	return true
}
