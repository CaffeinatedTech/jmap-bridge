package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// maxPushBody caps the push body: Pub/Sub notifications are tiny.
const maxPushBody = 1 << 20

// gmailNotification is the Gmail payload published to the Pub/Sub topic,
// base64-wrapped in the Pub/Sub message's data field.
type gmailNotification struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    string `json:"historyId"`
}

// pubsubPush is the Cloud Pub/Sub push envelope delivered to the endpoint.
type pubsubPush struct {
	Message struct {
		Data string `json:"data"`
	} `json:"message"`
}

// handleGmailPush receives a Pub/Sub push for account, authenticates it, and
// wakes the account's sync engine (FR-S.14). The push carries only a hint:
// the engine reads history from its own saved cursor, so a duplicate,
// replayed or even verified-but-forged hint cannot inject state (golden
// rule 1). It answers 2xx so Pub/Sub acks; a rejected credential is non-2xx.
func (s *Server) handleGmailPush(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	verifier, ok := s.gmailPush[account]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := verifier.Verify(r.Context(), r); err != nil {
		// The error text never contains the credential; do not echo the
		// request either.
		s.log.Warn("gmail: push rejected", "account", account, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := decodePush(r); err != nil {
		s.log.Warn("gmail: push body invalid", "account", account, "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if s.kick != nil {
		s.kick(account)
	}
	w.WriteHeader(http.StatusOK)
}

// decodePush validates the notification body. It accepts the real Pub/Sub
// envelope (`{"message":{"data":"<base64 JSON>"}}`) and the bare
// `{"emailAddress":…,"historyId":…}` form the loopback fixture sends. The
// parsed values are deliberately unused: the engine syncs from its own
// history cursor, making the hint idempotent.
func decodePush(r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPushBody))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	var env pubsubPush
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	if env.Message.Data == "" {
		var n gmailNotification
		return json.Unmarshal(body, &n)
	}
	raw, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err != nil {
		return err
	}
	var n gmailNotification
	return json.Unmarshal(raw, &n)
}
