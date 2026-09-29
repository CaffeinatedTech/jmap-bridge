package jmapapi

import (
	"context"
	"crypto/sha1" //nolint:gosec // queryState hashing, not security (PLAN §4.1)
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Request limits mirrored into the core capability object (RFC 8620 §2).
const (
	maxCallsInRequest = 256
	maxObjectsInGet   = 512
)

// capabilitiesAdvertised is the set of capability URNs this build
// implements; dispatch rejects a `using` entry outside it with an
// unknownCapability problem (FR-J.5). M0: core + mail only — submission
// lands with M3, contacts with M6, each gated on configuration.
var capabilitiesAdvertised = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:mail",
}

// Account is the per-request account context: the id every method's
// accountId argument must match, the Store that serves it, the Backend
// that mutates it (nil for a cache-only account, which must then refuse
// every write rather than pretend), and the session state echoed on API
// responses (RFC 8620 §3.4).
type Account struct {
	ID           string
	Store        Store
	Backend      Backend
	SessionState string
}

// methodFunc executes one method call. A non-nil *methodErr is emitted
// as an "error" invocation (RFC 8620 §3.6.2); any other error becomes a
// serverFail.
type methodFunc func(ctx context.Context, acct *Account, args json.RawMessage) (any, *methodErr)

// methodErr is an error object: short error code in Type (JMAP Error
// Codes registry, RFC 8620 §9.5.3), optional description, and — for the
// per-object failures a /set reports inside notCreated/notUpdated/
// notDestroyed — the offending property names (RFC 8620 §5.3).
type methodErr struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

func methodErrorf(typ, format string, args ...any) *methodErr {
	return &methodErr{Type: typ, Description: fmt.Sprintf(format, args...)}
}

// problem is a request-level error, a JSON problem-details object
// (RFC 8620 §3.6.1).
type problem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Limit  string `json:"limit,omitempty"`
}

// invocation is one methodResponses entry: [name, args, callId]
// (RFC 8620 §3.4).
type invocation struct {
	Name   string
	Args   any
	CallID string
}

func (i invocation) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]any{i.Name, i.Args, i.CallID})
}

// Response is the JMAP Response object (RFC 8620 §3.4).
type Response struct {
	MethodResponses []invocation `json:"methodResponses"`
	SessionState    string       `json:"sessionState"`
}

// Capabilities reports the capability URNs this build implements; the
// session resource advertises exactly this set (FR-J.5).
func Capabilities() []string {
	out := make([]string, len(capabilitiesAdvertised))
	copy(out, capabilitiesAdvertised)
	return out
}

// Handler dispatches JMAP batches against a Store.
type Handler struct {
	store Store
}

// NewHandler returns a Handler backed by store.
func NewHandler(store Store) *Handler {
	return &Handler{store: store}
}

// request is the decoded Request object (RFC 8620 §3.3). Pointers
// distinguish "absent" from "null" so a structurally incomplete request
// is a notRequest problem, not a silently defaulted one.
type request struct {
	Using       *[]string          `json:"using"`
	MethodCalls *[]json.RawMessage `json:"methodCalls"`
}

// call is one pre-validated methodCalls entry.
type call struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// Dispatch processes one API request body for acct and returns the HTTP
// status and response/problem object to marshal. Dispatch never panics
// and never answers 500 for a method-level failure (FR-J.2).
func (h *Handler) Dispatch(ctx context.Context, acct *Account, body []byte) (int, any) {
	if !json.Valid(body) {
		return 400, problem{
			Type: "urn:ietf:params:jmap:error:notJSON", Status: 400,
			Detail: "request body is not valid JSON",
		}
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return 400, problem{
			Type: "urn:ietf:params:jmap:error:notRequest", Status: 400,
			Detail: "body is not a Request object: " + err.Error(),
		}
	}
	if req.Using == nil || req.MethodCalls == nil {
		return 400, problem{
			Type: "urn:ietf:params:jmap:error:notRequest", Status: 400,
			Detail: "Request object requires using and methodCalls",
		}
	}
	for _, urn := range *req.Using {
		if !isAdvertised(urn) {
			return 400, problem{
				Type: "urn:ietf:params:jmap:error:unknownCapability", Status: 400,
				Detail: "server does not support capability " + urn,
			}
		}
	}
	if len(*req.MethodCalls) > maxCallsInRequest {
		return 400, problem{
			Type: "urn:ietf:params:jmap:error:limit", Status: 400,
			Detail: "too many method calls in one request", Limit: "maxCallsInRequest",
		}
	}

	calls := make([]call, 0, len(*req.MethodCalls))
	for _, raw := range *req.MethodCalls {
		c, err := decodeCall(raw)
		if err != nil {
			return 400, problem{
				Type: "urn:ietf:params:jmap:error:notRequest", Status: 400,
				Detail: err.Error(),
			}
		}
		calls = append(calls, c)
	}

	hdr := &dispatcher{h: h, acct: acct}
	for _, c := range calls {
		hdr.dispatch(ctx, c)
	}
	return 200, &Response{MethodResponses: hdr.out, SessionState: acct.SessionState}
}

// dispatcher carries per-request state: prior results for reference
// resolution and the response entries collected so far.
type dispatcher struct {
	h     *Handler
	acct  *Account
	out   []invocation
	names map[string]string // callId → method name
	args  map[string]any    // callId → decoded result object
}

func decodeCall(raw json.RawMessage) (call, error) {
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 3 {
		return call{}, fmt.Errorf("methodCalls entry must be a [name, args, callId] array")
	}
	var c call
	if err := json.Unmarshal(parts[0], &c.Name); err != nil || c.Name == "" {
		return call{}, fmt.Errorf("methodCalls entry has no method name")
	}
	if err := json.Unmarshal(parts[2], &c.CallID); err != nil {
		return call{}, fmt.Errorf("methodCalls entry has no call id")
	}
	c.Args = parts[1]
	return c, nil
}

func isAdvertised(urn string) bool {
	for _, a := range capabilitiesAdvertised {
		if a == urn {
			return true
		}
	}
	return false
}

// dispatch runs one call, resolving result references first (FR-J.3).
func (d *dispatcher) dispatch(ctx context.Context, c call) {
	var parsed any
	if err := json.Unmarshal(c.Args, &parsed); err != nil {
		d.fail(c.CallID, &methodErr{Type: "invalidArguments", Description: "arguments must be a JSON object"})
		return
	}
	args, merr := d.resolveValue(parsed)
	if merr != nil {
		d.fail(c.CallID, merr)
		return
	}
	raw, err := json.Marshal(args)
	if err != nil {
		d.fail(c.CallID, &methodErr{Type: "serverFail", Description: "could not decode arguments"})
		return
	}

	fn := d.h.method(c.Name)
	if fn == nil {
		d.fail(c.CallID, &methodErr{
			Type:        "unknownMethod",
			Description: "method " + c.Name + " is not implemented",
		})
		return
	}
	result, merr := fn(ctx, d.acct, raw)
	if merr != nil {
		d.fail(c.CallID, merr)
		return
	}

	// Normalise the result to plain JSON so later calls in this request
	// can resolve result references against it.
	var normalized any
	nb, err := json.Marshal(result)
	if err == nil {
		err = json.Unmarshal(nb, &normalized)
	}
	if err != nil {
		d.fail(c.CallID, &methodErr{Type: "serverFail", Description: "could not encode result"})
		return
	}
	if d.names == nil {
		d.names = map[string]string{}
		d.args = map[string]any{}
	}
	d.names[c.CallID] = c.Name
	d.args[c.CallID] = normalized
	d.out = append(d.out, invocation{Name: c.Name, Args: normalized, CallID: c.CallID})
}

func (d *dispatcher) fail(callID string, merr *methodErr) {
	d.out = append(d.out, invocation{Name: "error", Args: merr, CallID: callID})
}

// resultRef is the wire shape of an RFC 8620 §3.7 result reference.
type resultRef struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// resolveValue walks a decoded args tree, replacing any "#argument"
// result reference with the value it points at, re-keyed without the
// "#" so the method sees the real argument name (RFC 8620 §3.7,
// FR-J.3).
func (d *dispatcher) resolveValue(v any) (any, *methodErr) {
	switch t := v.(type) {
	case map[string]any:
		resolved := map[string]bool{}
		for key, val := range t {
			if !strings.HasPrefix(key, "#") {
				continue
			}
			name := key[1:]
			if name == "" {
				return nil, methodErrorf("invalidArguments", "argument name must not be just #")
			}
			if _, dup := t[name]; dup {
				return nil, methodErrorf("invalidArguments",
					"argument %q given in both normal and referenced form", name)
			}
			refMap, ok := val.(map[string]any)
			if !ok || !isResultRef(refMap) {
				return nil, methodErrorf("invalidArguments",
					"argument %q must be a ResultReference object", key)
			}
			out, merr := d.resolveRef(refMap)
			if merr != nil {
				return nil, merr
			}
			delete(t, key)
			t[name] = out
			resolved[name] = true
		}
		for k, val := range t {
			if resolved[k] {
				continue // resolved results are used verbatim
			}
			out, merr := d.resolveValue(val)
			if merr != nil {
				return nil, merr
			}
			t[k] = out
		}
		return t, nil
	case []any:
		for i, val := range t {
			out, merr := d.resolveValue(val)
			if merr != nil {
				return nil, merr
			}
			t[i] = out
		}
		return t, nil
	default:
		return v, nil
	}
}

func isResultRef(m map[string]any) bool {
	_, a := m["resultOf"]
	_, b := m["name"]
	_, c := m["path"]
	if !a || !b || !c {
		return false
	}
	_, aok := m["resultOf"].(string)
	_, bok := m["name"].(string)
	_, cok := m["path"].(string)
	return aok && bok && cok
}

func (d *dispatcher) resolveRef(m map[string]any) (any, *methodErr) {
	raw, _ := json.Marshal(m)
	var ref resultRef
	_ = json.Unmarshal(raw, &ref)

	result, ok := d.args[ref.ResultOf]
	if !ok {
		return nil, methodErrorf("invalidResultReference",
			"no prior call with id %q", ref.ResultOf)
	}
	if d.names[ref.ResultOf] != ref.Name {
		return nil, methodErrorf("invalidResultReference",
			"call %q is not a %s", ref.ResultOf, ref.Name)
	}
	resolved, err := jsonPointer(result, ref.Path)
	if err != nil {
		return nil, methodErrorf("invalidResultReference", "%s", err)
	}
	return resolved, nil
}

// jsonPointer resolves an RFC 6901 JSON pointer against a decoded JSON
// value (RFC 8620 §3.7).
func jsonPointer(v any, path string) (any, error) {
	if path == "" {
		return v, nil
	}
	if path[0] != '/' {
		return nil, fmt.Errorf("pointer %q must start with /", path)
	}
	cur := v
	for _, tok := range splitPointer(path[1:]) {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, fmt.Errorf("pointer %q does not resolve", path)
			}
			cur = next
		case []any:
			idx, err := atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("pointer %q does not resolve", path)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("pointer %q does not resolve", path)
		}
	}
	return cur, nil
}

func splitPointer(s string) []string {
	parts := make([]string, 0, 2)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			parts = append(parts, unescapeToken(s[start:i]))
			start = i + 1
		}
	}
	return append(parts, unescapeToken(s[start:]))
}

func unescapeToken(s string) string {
	if !containsTilde(s) {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '~' && i+1 < len(s) {
			switch s[i+1] {
			case '0':
				out = append(out, '~')
				i++
				continue
			case '1':
				out = append(out, '/')
				i++
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

func containsTilde(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			return true
		}
	}
	return false
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty index")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// queryState builds the queryState string: the Store's query counter
// plus a hash of the request arguments (PLAN §4.1), so it changes exactly
// when results may have changed (FR-X.7).
func queryState(counter string, args json.RawMessage) string {
	sum := sha1.Sum(args) //nolint:gosec // non-security hash, PLAN §4.1
	return counter + ":" + hex.EncodeToString(sum[:])[:16]
}
