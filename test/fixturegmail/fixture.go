// Package fixturegmail is the in-process Gmail API v1 server the gmailapi
// client and, from M10, the API backend tests run against (GMAIL_API_PLAN §12).
// It implements the subset of the REST API the bridge uses — profile, labels,
// messages (list/get/modify/delete/batch/send), history, threads, drafts,
// attachments, watch/stop and the batch endpoint — and, like fixtureimap, can
// impersonate the failure tiers: history expiry, quota throttling (including a
// batch whose sub-responses are all throttled under a 200 envelope), eventual
// visibility for own-write grace windows, and authentication refusal.
//
// It keeps its own small model with hand-written JSON shapes so the fixture
// does not depend on the generated Gmail types; wire correctness is proven by
// the client decoding these responses.
package fixturegmail

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tier selects a behavioural profile the fixture impersonates.
type Tier int

const (
	// TierNormal serves every implemented method faithfully.
	TierNormal Tier = iota
	// TierHistoryExpired answers history.list with 404 whenever a start
	// history id is supplied, as Google does when the cursor is too old.
	TierHistoryExpired
	// TierQuota throttles requests until ThrottleFirst have been refused,
	// each with Retry-After; batch sub-responses throttle under a 200 envelope.
	TierQuota
	// TierEventualVisibility accepts a label mutation but withholds its
	// history record until ReleaseHistory, modelling Gmail's own-write lag.
	TierEventualVisibility
	// TierUnauthorized refuses every request with 401.
	TierUnauthorized
)

// Options configures a fixture server.
type Options struct {
	Tier Tier
	// User is the account address; default "fixture@example.test".
	User string
	// Token is the bearer token the server requires; default "fixture-token".
	Token string
	// ThrottleFirst is how many requests TierQuota refuses before serving;
	// default 1. Throttling still applies while the count is unmet.
	ThrottleFirst int
	// RetryAfter is the Retry-After header value on a throttle; default 1s.
	RetryAfter time.Duration
}

// Well-known label ids the fixture seeds and asserts on. Kept local so the
// fixture does not depend on the client package's constants.
const (
	LabelInbox     = "INBOX"
	LabelSent      = "SENT"
	LabelDraft     = "DRAFT"
	LabelTrash     = "TRASH"
	LabelSpam      = "SPAM"
	LabelUnread    = "UNREAD"
	LabelStarred   = "STARRED"
	LabelImportant = "IMPORTANT"
)

// Header is a MIME header pair.
type Header struct {
	Name  string
	Value string
}

// SeedMessage describes a message to place in the fixture's mailbox.
type SeedMessage struct {
	ID            string
	ThreadID      string
	LabelIDs      []string
	InternalDate  int64
	SizeEstimate  int64
	Snippet       string
	Raw           []byte
	Headers       []Header
	HasAttachment bool
}

type message struct {
	id            string
	threadID      string
	labelIDs      []string
	internalDate  int64
	sizeEstimate  int64
	snippet       string
	raw           []byte
	headers       []Header
	hasAttachment bool
	historyID     uint64
}

type label struct {
	id             string
	name           string
	typ            string
	messagesTotal  int64
	messagesUnread int64
	threadsTotal   int64
	threadsUnread  int64
}

type historyRecord struct {
	id            uint64
	added         []string
	deleted       []string
	labelsAdded   map[string][]string
	labelsRemoved map[string][]string
}

type sentMessage struct {
	Raw []byte
	To  []string
}

// Server is a running fixture Gmail API server.
type Server struct {
	t     testing.TB
	srv   *httptest.Server
	user  string
	token string
	tier  Tier

	throttleFirst int
	retryAfter    time.Duration

	mu             sync.Mutex
	historyID      uint64
	labels         map[string]*label
	labelOrder     []string
	messages       map[string]*message
	messageOrder   []string
	history        []historyRecord
	pendingHistory []historyRecord
	throttled      int
	attachments    map[string][]byte
	sent           []sentMessage
	draftSeq       int
	drafts         map[string]string // draft id -> message id
}

// Start brings a fixture server up on loopback and stops it on test cleanup.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.User == "" {
		opts.User = "fixture@example.test"
	}
	if opts.Token == "" {
		opts.Token = "fixture-token"
	}
	if opts.ThrottleFirst == 0 {
		opts.ThrottleFirst = 1
	}
	if opts.RetryAfter == 0 {
		opts.RetryAfter = time.Second
	}
	s := &Server{
		t:             t,
		user:          opts.User,
		token:         opts.Token,
		tier:          opts.Tier,
		throttleFirst: opts.ThrottleFirst,
		retryAfter:    opts.RetryAfter,
		historyID:     1000,
		labels:        map[string]*label{},
		messages:      map[string]*message{},
		attachments:   map[string][]byte{},
		drafts:        map[string]string{},
		throttled:     0,
	}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base URL to hand to gmailapi.Options.Endpoint (trailing slash).
func (s *Server) URL() string { return s.srv.URL + "/" }

// Token is the bearer token the fixture requires.
func (s *Server) Token() string { return s.token }

// User is the account address the fixture reports.
func (s *Server) User() string { return s.user }

// SeedLabel adds a label without recording history (initial state).
func (s *Server) SeedLabel(id, name, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLabelLocked(id, name, typ)
}

// SeedMessage adds a message without recording history and returns its id.
func (s *Server) SeedMessage(m SeedMessage) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putMessageLocked(m)
}

// AddAttachment stores attachment bytes under an id, served by
// messages.attachments.get.
func (s *Server) AddAttachment(id string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachments[id] = append([]byte(nil), data...)
}

// ForeignAddMessage adds a message as an external client would, recording
// history for the incremental sync path. It returns the message id.
func (s *Server) ForeignAddMessage(m SeedMessage) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.putMessageLocked(m)
	rec := s.newHistoryLocked()
	rec.added = append(rec.added, id)
	s.appendHistoryLocked(rec)
	return id
}

// ForeignModifyLabels changes a message's labels as an external client would.
func (s *Server) ForeignModifyLabels(id string, add, remove []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.messages[id]
	if m == nil {
		return
	}
	rec := s.newHistoryLocked()
	s.applyLabelsLocked(m, add, remove)
	if len(add) > 0 {
		if rec.labelsAdded == nil {
			rec.labelsAdded = map[string][]string{}
		}
		rec.labelsAdded[id] = append(rec.labelsAdded[id], add...)
	}
	if len(remove) > 0 {
		if rec.labelsRemoved == nil {
			rec.labelsRemoved = map[string][]string{}
		}
		rec.labelsRemoved[id] = append(rec.labelsRemoved[id], remove...)
	}
	s.appendHistoryLocked(rec)
}

// ForeignDeleteMessage permanently removes a message as an external client
// would, recording history.
func (s *Server) ForeignDeleteMessage(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages[id] == nil {
		return
	}
	delete(s.messages, id)
	s.removeOrderLocked(id)
	rec := s.newHistoryLocked()
	rec.deleted = append(rec.deleted, id)
	s.appendHistoryLocked(rec)
}

// ReleaseHistory flushes history records withheld by TierEventualVisibility.
func (s *Server) ReleaseHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, s.pendingHistory...)
	s.pendingHistory = nil
}

// Sent returns the raw messages the fixture accepted through messages.send.
func (s *Server) Sent() []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// CurrentHistoryID returns the mailbox's current history id.
func (s *Server) CurrentHistoryID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyID
}

// --- model helpers (callers hold the lock) ---

func (s *Server) putLabelLocked(id, name, typ string) {
	if typ == "" {
		typ = "user"
	}
	if _, ok := s.labels[id]; !ok {
		s.labelOrder = append(s.labelOrder, id)
	}
	s.labels[id] = &label{id: id, name: name, typ: typ}
}

func (s *Server) putMessageLocked(m SeedMessage) string {
	if m.ID == "" {
		m.ID = fmt.Sprintf("msg-%d", len(s.messages)+1)
	}
	if m.ThreadID == "" {
		m.ThreadID = "thr-" + m.ID
	}
	size := m.SizeEstimate
	if size == 0 {
		size = int64(len(m.Raw))
	}
	internal := m.InternalDate
	if internal == 0 {
		internal = time.Now().UnixMilli()
	}
	if _, ok := s.messages[m.ID]; !ok {
		s.messageOrder = append(s.messageOrder, m.ID)
	}
	s.messages[m.ID] = &message{
		id:            m.ID,
		threadID:      m.ThreadID,
		labelIDs:      append([]string(nil), m.LabelIDs...),
		internalDate:  internal,
		sizeEstimate:  size,
		snippet:       m.Snippet,
		raw:           append([]byte(nil), m.Raw...),
		headers:       append([]Header(nil), m.Headers...),
		hasAttachment: m.HasAttachment,
		historyID:     s.historyID,
	}
	return m.ID
}

func (s *Server) newHistoryLocked() historyRecord {
	s.historyID++
	return historyRecord{id: s.historyID}
}

func (s *Server) appendHistoryLocked(rec historyRecord) {
	if s.tier == TierEventualVisibility {
		s.pendingHistory = append(s.pendingHistory, rec)
		return
	}
	s.history = append(s.history, rec)
}

func (s *Server) applyLabelsLocked(m *message, add, remove []string) {
	removeSet := map[string]bool{}
	for _, id := range remove {
		removeSet[id] = true
	}
	var kept []string
	for _, id := range m.labelIDs {
		if !removeSet[id] {
			kept = append(kept, id)
		}
	}
	have := map[string]bool{}
	for _, id := range kept {
		have[id] = true
	}
	for _, id := range add {
		if !have[id] {
			kept = append(kept, id)
			have[id] = true
		}
	}
	m.labelIDs = kept
	m.historyID = s.historyID + 1
}

func (s *Server) removeOrderLocked(id string) {
	for i, v := range s.messageOrder {
		if v == id {
			s.messageOrder = append(s.messageOrder[:i], s.messageOrder[i+1:]...)
			return
		}
	}
}

// --- HTTP ---

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.tier == TierUnauthorized || r.Header.Get("Authorization") != "Bearer "+s.token {
		writeError(w, http.StatusUnauthorized, "Invalid Credentials", "authError")
		return
	}
	s.dispatch(w, r)
}

// dispatch routes an already-authenticated request.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/batch/gmail/v1" {
		s.handleBatch(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/gmail/v1/users/") {
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
		return
	}
	if s.throttle() {
		s.writeThrottle(w)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/")
	segs := strings.Split(rest, "/")
	if len(segs) < 2 {
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
		return
	}
	s.routeUser(w, r, segs[1:])
}

// throttle reports (and counts) whether this request should be refused.
func (s *Server) throttle() bool {
	if s.tier != TierQuota {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.throttled >= s.throttleFirst {
		return false
	}
	s.throttled++
	return true
}

func (s *Server) writeThrottle(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(s.retryAfter.Seconds())))
	writeError(w, http.StatusTooManyRequests, "User-rate limit exceeded.", "rateLimitExceeded")
}

func (s *Server) routeUser(w http.ResponseWriter, r *http.Request, segs []string) {
	switch segs[0] {
	case "profile":
		s.handleProfile(w, r)
	case "labels":
		s.handleLabels(w, r, segs[1:])
	case "messages":
		s.handleMessages(w, r, segs[1:])
	case "threads":
		s.handleThreads(w, r, segs[1:])
	case "history":
		s.handleHistory(w, r)
	case "drafts":
		s.handleDrafts(w, r, segs[1:])
	case "watch":
		s.handleWatch(w, r)
	case "stop":
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
	}
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	total := 0
	seenThreads := map[string]bool{}
	for _, m := range s.messages {
		total++
		seenThreads[m.threadID] = true
	}
	hist := s.historyID
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"emailAddress":  s.user,
		"messagesTotal": total,
		"threadsTotal":  len(seenThreads),
		"historyId":     strconv.FormatUint(hist, 10),
	})
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 0 && r.Method == http.MethodGet:
		s.mu.Lock()
		out := make([]map[string]any, 0, len(s.labelOrder))
		for _, id := range s.labelOrder {
			out = append(out, labelJSON(s.labels[id]))
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"labels": out})
	case len(segs) == 0 && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeError(w, http.StatusBadRequest, "Invalid label name", "invalidArgument")
			return
		}
		id := "Label_" + strconv.Itoa(len(s.labels)+1)
		s.mu.Lock()
		s.putLabelLocked(id, body.Name, "user")
		l := s.labels[id]
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, labelJSON(l))
	case len(segs) == 1 && r.Method == http.MethodGet:
		s.mu.Lock()
		l := s.labels[segs[0]]
		s.mu.Unlock()
		if l == nil {
			writeError(w, http.StatusNotFound, "Not Found", "notFound")
			return
		}
		writeJSON(w, http.StatusOK, labelJSON(l))
	case len(segs) == 1 && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		l := s.labels[segs[0]]
		if l != nil && body.Name != "" {
			l.name = body.Name
		}
		s.mu.Unlock()
		if l == nil {
			writeError(w, http.StatusNotFound, "Not Found", "notFound")
			return
		}
		writeJSON(w, http.StatusOK, labelJSON(l))
	case len(segs) == 1 && r.Method == http.MethodDelete:
		s.mu.Lock()
		_, ok := s.labels[segs[0]]
		delete(s.labels, segs[0])
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "Not Found", "notFound")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
	}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 0 && r.Method == http.MethodGet:
		s.listMessages(w, r)
	case len(segs) == 1 && segs[0] == "send" && r.Method == http.MethodPost:
		s.sendMessage(w, r)
	case len(segs) == 1 && segs[0] == "batchModify" && r.Method == http.MethodPost:
		s.batchModify(w, r)
	case len(segs) == 1 && segs[0] == "batchDelete" && r.Method == http.MethodPost:
		s.batchDelete(w, r)
	case len(segs) == 1 && r.Method == http.MethodGet:
		s.getMessage(w, r, segs[0])
	case len(segs) == 1 && r.Method == http.MethodDelete:
		s.deleteMessage(w, segs[0])
	case len(segs) == 2 && segs[1] == "modify" && r.Method == http.MethodPost:
		s.modifyMessage(w, r, segs[0])
	case len(segs) == 3 && segs[1] == "attachments" && r.Method == http.MethodGet:
		s.getAttachment(w, segs[0], segs[2])
	default:
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
	}
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	labelIDs := q["labelIds"]
	query := q.Get("q")
	offset := 0
	if pt := q.Get("pageToken"); pt != "" {
		offset, _ = strconv.Atoi(pt)
	}
	max := 100
	if mr := q.Get("maxResults"); mr != "" {
		if v, err := strconv.Atoi(mr); err == nil && v > 0 {
			max = v
		}
	}

	s.mu.Lock()
	var ids []string
	for _, id := range s.messageOrder {
		m := s.messages[id]
		if m == nil || !matchesLabels(m, labelIDs) || !matchesQuery(m, query) {
			continue
		}
		ids = append(ids, id)
	}
	s.mu.Unlock()

	page := []map[string]any{}
	next := ""
	for i := offset; i < len(ids) && i < offset+max; i++ {
		m := s.messages[ids[i]]
		page = append(page, map[string]any{"id": ids[i], "threadId": m.threadID})
	}
	if offset+max < len(ids) {
		next = strconv.Itoa(offset + max)
	}
	resp := map[string]any{"messages": page, "resultSizeEstimate": len(ids)}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request, id string) {
	format := r.URL.Query().Get("format")
	metadataHeaders := r.URL.Query()["metadataHeaders"]
	s.mu.Lock()
	m := s.messages[id]
	s.mu.Unlock()
	if m == nil {
		writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
		return
	}
	writeJSON(w, http.StatusOK, messageJSON(m, format, metadataHeaders))
}

func (s *Server) modifyMessage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		AddLabelIds    []string `json:"addLabelIds"`
		RemoveLabelIds []string `json:"removeLabelIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request", "invalidArgument")
		return
	}
	s.mu.Lock()
	m := s.messages[id]
	if m == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
		return
	}
	rec := s.newHistoryLocked()
	s.applyLabelsLocked(m, body.AddLabelIds, body.RemoveLabelIds)
	if len(body.AddLabelIds) > 0 {
		rec.labelsAdded = map[string][]string{id: body.AddLabelIds}
	}
	if len(body.RemoveLabelIds) > 0 {
		rec.labelsRemoved = map[string][]string{id: body.RemoveLabelIds}
	}
	s.appendHistoryLocked(rec)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, messageJSON(m, "", nil))
}

func (s *Server) batchModify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ids            []string `json:"ids"`
		AddLabelIds    []string `json:"addLabelIds"`
		RemoveLabelIds []string `json:"removeLabelIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request", "invalidArgument")
		return
	}
	s.mu.Lock()
	rec := s.newHistoryLocked()
	for _, id := range body.Ids {
		if m := s.messages[id]; m != nil {
			s.applyLabelsLocked(m, body.AddLabelIds, body.RemoveLabelIds)
			if len(body.AddLabelIds) > 0 {
				if rec.labelsAdded == nil {
					rec.labelsAdded = map[string][]string{}
				}
				rec.labelsAdded[id] = append(rec.labelsAdded[id], body.AddLabelIds...)
			}
			if len(body.RemoveLabelIds) > 0 {
				if rec.labelsRemoved == nil {
					rec.labelsRemoved = map[string][]string{}
				}
				rec.labelsRemoved[id] = append(rec.labelsRemoved[id], body.RemoveLabelIds...)
			}
		}
	}
	s.appendHistoryLocked(rec)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ids []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request", "invalidArgument")
		return
	}
	s.mu.Lock()
	rec := s.newHistoryLocked()
	for _, id := range body.Ids {
		if _, ok := s.messages[id]; ok {
			delete(s.messages, id)
			s.removeOrderLocked(id)
			rec.deleted = append(rec.deleted, id)
		}
	}
	s.appendHistoryLocked(rec)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteMessage(w http.ResponseWriter, id string) {
	s.mu.Lock()
	if _, ok := s.messages[id]; !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
		return
	}
	delete(s.messages, id)
	s.removeOrderLocked(id)
	rec := s.newHistoryLocked()
	rec.deleted = append(rec.deleted, id)
	s.appendHistoryLocked(rec)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request", "invalidArgument")
		return
	}
	raw, err := base64.URLEncoding.DecodeString(body.Raw)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(body.Raw)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid raw message", "invalidArgument")
		return
	}
	s.mu.Lock()
	id := s.putMessageLocked(SeedMessage{
		LabelIDs:     []string{LabelSent},
		Raw:          raw,
		InternalDate: time.Now().UnixMilli(),
	})
	s.sent = append(s.sent, sentMessage{Raw: raw})
	rec := s.newHistoryLocked()
	rec.added = append(rec.added, id)
	s.appendHistoryLocked(rec)
	m := s.messages[id]
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "threadId": m.threadID, "labelIds": m.labelIDs})
}

func (s *Server) getAttachment(w http.ResponseWriter, _ string, attachmentID string) {
	s.mu.Lock()
	data := s.attachments[attachmentID]
	s.mu.Unlock()
	if data == nil {
		writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attachmentId": attachmentID,
		"size":         len(data),
		"data":         base64.RawURLEncoding.EncodeToString(data),
	})
}

func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) != 1 || r.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
		return
	}
	threadID := segs[0]
	s.mu.Lock()
	var msgs []map[string]any
	for _, id := range s.messageOrder {
		if m := s.messages[id]; m != nil && m.threadID == threadID {
			msgs = append(msgs, messageJSON(m, "", nil))
		}
	}
	hist := s.historyID
	s.mu.Unlock()
	if len(msgs) == 0 {
		writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        threadID,
		"historyId": strconv.FormatUint(hist, 10),
		"messages":  msgs,
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	start := r.URL.Query().Get("startHistoryId")
	if start == "" {
		writeError(w, http.StatusBadRequest, "startHistoryId is required", "invalidArgument")
		return
	}
	startID, err := strconv.ParseUint(start, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid startHistoryId", "invalidArgument")
		return
	}
	if s.tier == TierHistoryExpired {
		writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
		return
	}
	s.mu.Lock()
	var records []map[string]any
	for _, rec := range s.history {
		if rec.id > startID {
			records = append(records, historyJSON(rec))
		}
	}
	hist := s.historyID
	s.mu.Unlock()
	if records == nil {
		records = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"history":   records,
		"historyId": strconv.FormatUint(hist, 10),
	})
}

func (s *Server) handleDrafts(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 0 && r.Method == http.MethodGet:
		s.mu.Lock()
		out := make([]map[string]any, 0, len(s.drafts))
		for id, msgID := range s.drafts {
			out = append(out, map[string]any{"id": id, "message": map[string]any{"id": msgID}})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"drafts": out})
	case len(segs) == 0 && r.Method == http.MethodPost:
		var body struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, err := base64.URLEncoding.DecodeString(body.Message.Raw)
		if err != nil {
			raw, err = base64.RawURLEncoding.DecodeString(body.Message.Raw)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid raw message", "invalidArgument")
			return
		}
		s.mu.Lock()
		msgID := s.putMessageLocked(SeedMessage{LabelIDs: []string{LabelDraft}, Raw: raw})
		s.draftSeq++
		draftID := "draft-" + strconv.Itoa(s.draftSeq)
		s.drafts[draftID] = msgID
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": draftID, "message": map[string]any{"id": msgID}})
	case len(segs) == 1 && r.Method == http.MethodGet:
		s.mu.Lock()
		msgID := s.drafts[segs[0]]
		s.mu.Unlock()
		if msgID == "" {
			writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": segs[0], "message": map[string]any{"id": msgID}})
	case len(segs) == 1 && segs[0] == "send" && r.Method == http.MethodPost:
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		msgID := s.drafts[body.ID]
		m := s.messages[msgID]
		if m != nil {
			m.labelIDs = []string{LabelSent}
			s.sent = append(s.sent, sentMessage{Raw: m.raw})
		}
		delete(s.drafts, body.ID)
		s.mu.Unlock()
		if m == nil {
			writeError(w, http.StatusNotFound, "Requested entity was not found.", "notFound")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": msgID, "threadId": m.threadID, "labelIds": m.labelIDs})
	default:
		writeError(w, http.StatusNotFound, "Not Found", "notFound")
	}
}

func (s *Server) handleWatch(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	hist := s.historyID
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"historyId":  strconv.FormatUint(hist, 10),
		"expiration": strconv.FormatInt(time.Now().Add(7*24*time.Hour).UnixMilli(), 10),
	})
}

// --- batch endpoint ---

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed", "methodNotAllowed")
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Missing batch boundary", "invalidArgument")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			writeError(w, http.StatusBadRequest, "Bad batch part", "invalidArgument")
			return
		}
		inner, rerr := http.ReadRequest(bufio.NewReader(part))
		if rerr != nil {
			_ = part.Close()
			writeError(w, http.StatusBadRequest, "Bad inner request", "invalidArgument")
			return
		}
		contentID := part.Header.Get("Content-ID")
		_ = part.Close()

		rec := httptest.NewRecorder()
		// Re-dispatch behind the same auth context; sub-requests carry no
		// Authorization header of their own.
		inner.RemoteAddr = r.RemoteAddr
		s.dispatch(rec, inner)

		headers := textprotoMIMEHeader(contentID)
		wpart, werr := mw.CreatePart(headers)
		if werr != nil {
			writeError(w, http.StatusInternalServerError, "Batch write failed", "internalError")
			return
		}
		writeInnerResponse(wpart, rec)
	}
	_ = mw.Close()
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, buf.String())
}

func writeInnerResponse(w io.Writer, rec *httptest.ResponseRecorder) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", rec.Code, http.StatusText(rec.Code))
	for k, vs := range rec.Header() {
		for _, v := range vs {
			_, _ = fmt.Fprintf(w, "%s: %s\r\n", k, v)
		}
	}
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "" {
		_, _ = fmt.Fprint(w, "Content-Type: application/json; charset=UTF-8\r\n")
	}
	_, _ = fmt.Fprint(w, "\r\n")
	_, _ = w.Write(rec.Body.Bytes())
}

// --- JSON shapes ---

func labelJSON(l *label) map[string]any {
	return map[string]any{
		"id":             l.id,
		"name":           l.name,
		"type":           l.typ,
		"messagesTotal":  l.messagesTotal,
		"messagesUnread": l.messagesUnread,
		"threadsTotal":   l.threadsTotal,
		"threadsUnread":  l.threadsUnread,
	}
}

func messageJSON(m *message, format string, metadataHeaders []string) map[string]any {
	out := map[string]any{
		"id":           m.id,
		"threadId":     m.threadID,
		"labelIds":     m.labelIDs,
		"historyId":    strconv.FormatUint(m.historyID, 10),
		"internalDate": strconv.FormatInt(m.internalDate, 10),
		"sizeEstimate": m.sizeEstimate,
		"snippet":      m.snippet,
	}
	if format == "raw" {
		out["raw"] = base64.RawURLEncoding.EncodeToString(m.raw)
		return out
	}
	headers := m.headers
	if len(metadataHeaders) > 0 {
		want := map[string]bool{}
		for _, h := range metadataHeaders {
			want[strings.ToLower(h)] = true
		}
		var filtered []Header
		for _, h := range headers {
			if want[strings.ToLower(h.Name)] {
				filtered = append(filtered, h)
			}
		}
		headers = filtered
	}
	hjson := make([]map[string]any, 0, len(headers))
	for _, h := range headers {
		hjson = append(hjson, map[string]any{"name": h.Name, "value": h.Value})
	}
	out["payload"] = map[string]any{
		"mimeType": "text/plain",
		"headers":  hjson,
	}
	return out
}

func historyJSON(rec historyRecord) map[string]any {
	out := map[string]any{"id": strconv.FormatUint(rec.id, 10)}
	if len(rec.added) > 0 {
		added := make([]map[string]any, 0, len(rec.added))
		for _, id := range rec.added {
			added = append(added, map[string]any{"message": map[string]any{"id": id}})
		}
		out["messagesAdded"] = added
	}
	if len(rec.deleted) > 0 {
		deleted := make([]map[string]any, 0, len(rec.deleted))
		for _, id := range rec.deleted {
			deleted = append(deleted, map[string]any{"message": map[string]any{"id": id}})
		}
		out["messagesDeleted"] = deleted
	}
	if len(rec.labelsAdded) > 0 {
		var list []map[string]any
		for id, labels := range rec.labelsAdded {
			list = append(list, map[string]any{
				"message":  map[string]any{"id": id},
				"labelIds": labels,
			})
		}
		out["labelsAdded"] = list
	}
	if len(rec.labelsRemoved) > 0 {
		var list []map[string]any
		for id, labels := range rec.labelsRemoved {
			list = append(list, map[string]any{
				"message":  map[string]any{"id": id},
				"labelIds": labels,
			})
		}
		out["labelsRemoved"] = list
	}
	return out
}

// --- matching and helpers ---

func matchesLabels(m *message, want []string) bool {
	if len(want) == 0 {
		return true
	}
	have := map[string]bool{}
	for _, id := range m.labelIDs {
		have[id] = true
	}
	for _, id := range want {
		if !have[id] {
			return false
		}
	}
	return true
}

func matchesQuery(m *message, q string) bool {
	if q == "" {
		return true
	}
	for _, term := range strings.Fields(q) {
		switch {
		case term == "has:attachment":
			if !m.hasAttachment {
				return false
			}
		case strings.HasPrefix(term, "label:"):
			if !matchesLabels(m, []string{strings.TrimPrefix(term, "label:")}) {
				return false
			}
		case strings.HasPrefix(term, "rfc822msgid:"):
			want := strings.TrimPrefix(term, "rfc822msgid:")
			if !strings.EqualFold(header(m.headers, "Message-ID"), want) {
				return false
			}
		default:
			haystack := strings.ToLower(header(m.headers, "Subject") + " " + m.snippet + " " + header(m.headers, "From") + " " + header(m.headers, "To"))
			if !strings.Contains(haystack, strings.ToLower(term)) {
				return false
			}
		}
	}
	return true
}

func header(headers []Header, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func textprotoMIMEHeader(contentID string) map[string][]string {
	h := map[string][]string{
		"Content-Type":              {"application/http"},
		"Content-Transfer-Encoding": {"binary"},
	}
	if contentID == "" {
		contentID = "<response-unknown>"
	}
	if !strings.Contains(contentID, "response-") {
		contentID = strings.Replace(contentID, "<", "<response-", 1)
	}
	h["Content-ID"] = []string{contentID}
	return h
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, reason string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"errors": []map[string]any{{
				"message": message,
				"domain":  "global",
				"reason":  reason,
			}},
		},
	})
}
