package jmapapi

import (
	"context"
	"crypto/sha1" //nolint:gosec // queryState hashing, not security (PLAN §4.1)
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/CaffeinatedTech/jmap-bridge/internal/metrics"
)

// Request limits mirrored into the core capability object (RFC 8620 §2).
const (
	maxCallsInRequest = 256
	maxObjectsInGet   = 512
)

// capabilitiesAdvertised is the set of capability URNs this build
// implements; dispatch rejects a `using` entry outside it with an
// unknownCapability problem (FR-J.5). Whether an account actually
// offers one is narrower and lives on [Account.Capabilities]: M3's
// submission needs SMTP configured, M6's contacts need CardDAV.
var capabilitiesAdvertised = []string{
	"urn:ietf:params:jmap:core",
	"urn:ietf:params:jmap:mail",
	"urn:ietf:params:jmap:submission",
	ContactURN,
}

// Account is the per-request account context: the id every method's
// accountId argument must match, the Store that serves it, the Backend
// that mutates it (nil for a cache-only account, which must then refuse
// every write rather than pretend), the capability URNs this account
// offers (FR-J.5), the sendable Identity it answers Identity/get with
// (nil when submission is not configured, FR-M.14), and the session
// state echoed on API responses (RFC 8620 §3.4).
type Account struct {
	ID           string
	Store        Store
	Backend      Backend
	Capabilities []string
	Identity     *Identity
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
	// InvalidRecipients carries the offending addresses an
	// invalidRecipients error must list (RFC 8621 §7.5).
	InvalidRecipients []string `json:"invalidRecipients,omitempty"`
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
	// methodCalls and methodErrors are nil unless WithMetrics was given;
	// the dispatcher checks before every use so a metrics-free Handler
	// pays nothing (FR-D.6).
	methodCalls  *metrics.CounterVec
	methodErrors *metrics.CounterVec
}

// Option configures an optional Handler collaborator.
type Option func(*Handler)

// WithMetrics attaches a registry for the FR-D.6 method counters. A nil
// registry leaves metrics off.
func WithMetrics(reg *metrics.Registry) Option {
	return func(h *Handler) {
		if reg != nil {
			h.methodCalls = reg.Counter("jmap_bridge_method_calls_total",
				"JMAP method invocations, by account and method.", "account", "method")
			h.methodErrors = reg.Counter("jmap_bridge_method_errors_total",
				"JMAP method errors, by account, method and error type.", "account", "method", "type")
		}
	}
}

// NewHandler returns a Handler backed by store.
func NewHandler(store Store, opts ...Option) *Handler {
	h := &Handler{store: store}
	for _, opt := range opts {
		opt(h)
	}
	return h
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
		if !accountOffers(acct, urn) {
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
	using := make(map[string]bool, len(*req.Using))
	for _, urn := range *req.Using {
		using[urn] = true
	}
	hdr.using = using
	for _, c := range calls {
		hdr.dispatch(withCallScope(ctx, hdr, c.CallID), c)
	}
	return 200, &Response{MethodResponses: hdr.out, SessionState: acct.SessionState}
}

// callResult is a method's answer when its response must be followed by
// a second invocation: RFC 8621 §7.5 requires the implicit Email/set
// that carries EmailSubmission/set's onSuccess* effects to come after
// the EmailSubmission/set response itself.
type callResult struct {
	Value    any
	Followup *invocation
}

// callScope is the per-call request context a method may need: its own
// call id (so a follow-up invocation can answer the same one, what
// Stalwart does — PLAN §2.1) and the batch's creation-reference
// resolver. Both belong to the dispatcher, so they travel through the
// context rather than through every method signature.
type callScope struct {
	callID  string
	resolve func(ref string) (string, bool)
}

type callScopeKey struct{}

func withCallScope(ctx context.Context, d *dispatcher, callID string) context.Context {
	return context.WithValue(ctx, callScopeKey{}, &callScope{
		callID:  callID,
		resolve: d.resolveCreation,
	})
}

// callScopeOf returns the scope the dispatcher bound, or nil outside a
// dispatch (tests calling a handler directly).
func callScopeOf(ctx context.Context) *callScope {
	scope, _ := ctx.Value(callScopeKey{}).(*callScope)
	return scope
}

// resolveCreationRef resolves a creation reference ("#handle") against
// the results of earlier calls in this request. An id without the "#"
// prefix is not a reference and does not resolve.
func resolveCreationRef(ctx context.Context, ref string) (string, bool) {
	if scope := callScopeOf(ctx); scope != nil && scope.resolve != nil {
		return scope.resolve(ref)
	}
	return "", false
}

// dispatcher carries per-request state: prior results for reference
// resolution and the response entries collected so far.
type dispatcher struct {
	h     *Handler
	acct  *Account
	out   []invocation
	names map[string]string // callId → method name
	args  map[string]any    // callId → decoded result object
	// using is the request's declared capability set: a method whose
	// capability is absent is unknownMethod (RFC 8620 §3.3).
	using map[string]bool
	// method is the call currently being dispatched; fail reports it on
	// the error counter even when its arguments never parsed.
	method string
}

// Capability URNs a method can belong to (RFC 8620 §3.3). Core is not
// assumed: an empty using makes even Core/echo unknownMethod.
const (
	coreURN       = "urn:ietf:params:jmap:core"
	mailURN       = "urn:ietf:params:jmap:mail"
	submissionURN = "urn:ietf:params:jmap:submission"
)

// capabilityForMethod names the capability a method requires; "" means
// the name is not a known method (the lookup will answer unknownMethod).
func capabilityForMethod(name string) string {
	switch {
	case strings.HasPrefix(name, "Core/"):
		return coreURN
	case strings.HasPrefix(name, "Mailbox/"), strings.HasPrefix(name, "Email/"),
		strings.HasPrefix(name, "Thread/"):
		return mailURN
	case strings.HasPrefix(name, "Identity/"), strings.HasPrefix(name, "EmailSubmission/"):
		return submissionURN
	case strings.HasPrefix(name, "AddressBook/"), strings.HasPrefix(name, "ContactCard/"):
		return ContactURN
	default:
		return ""
	}
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

// accountOffers reports whether this account may use urn: the build
// must implement it *and* the account must offer it. The second check
// is what keeps capability honesty honest (FR-J.5): submission is
// known to the build but unknown to an account with no SMTP, so a
// request naming it gets the same unknownCapability answer a client
// sees for a URN nobody implements.
func accountOffers(acct *Account, urn string) bool {
	if !isAdvertised(urn) {
		return false
	}
	for _, a := range acct.Capabilities {
		if a == urn {
			return true
		}
	}
	return false
}

// dispatch runs one call, resolving result references first (FR-J.3).
func (d *dispatcher) dispatch(ctx context.Context, c call) {
	d.method = c.Name
	if d.h.methodCalls != nil {
		// The method name is counted even when it turns out to be
		// unknown: a client hammering a typo is a signal too.
		d.h.methodCalls.With(d.acct.ID, c.Name).Inc()
	}
	// RFC 8620 §3.3: a method requires its capability to be declared in
	// using. Core is not implicit — an empty using refuses every method.
	// Only gate capabilities the account actually offers; a method whose
	// capability is unconfigured must reach its handler and answer its
	// specific error (e.g. accountNotSupportedByMethod for submission).
	if urn := capabilityForMethod(c.Name); urn != "" && !d.using[urn] && accountOffers(d.acct, urn) {
		d.fail(c.CallID, &methodErr{
			Type:        "unknownMethod",
			Description: "method " + c.Name + " requires capability " + urn + " not declared in using",
		})
		return
	}
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
	var followup *invocation
	if r, ok := result.(*callResult); ok {
		result, followup = r.Value, r.Followup
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
	// The follow-up rides the response list but is not registered for
	// reference resolution: it answers the same call id as the method
	// that triggered it (what Stalwart does, PLAN §2.1), and shadowing
	// the method's own result would break a "#handle" reference to it.
	if followup != nil {
		d.out = append(d.out, *followup)
	}
}

// resolveCreation maps "#handle" to the id an earlier call created
// under that handle. Every prior result's `created` map is consulted —
// the shape a client means when it points a submission at a draft it
// created in the same batch — and the most recent one wins, since call
// order is the batch's own.
func (d *dispatcher) resolveCreation(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "#") {
		return "", false
	}
	handle := ref[1:]
	if handle == "" {
		return "", false
	}
	found := ""
	for _, inv := range d.out {
		obj, ok := inv.Args.(map[string]any)
		if !ok {
			continue
		}
		created, ok := obj["created"].(map[string]any)
		if !ok {
			continue
		}
		entry, ok := created[handle].(map[string]any)
		if !ok {
			continue
		}
		if id, ok := entry["id"].(string); ok && id != "" {
			found = id
		}
	}
	return found, found != ""
}

func (d *dispatcher) fail(callID string, merr *methodErr) {
	if d.h.methodErrors != nil {
		d.h.methodErrors.With(d.acct.ID, d.method, merr.Type).Inc()
	}
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
			// Only a ResultReference object is a result reference
			// (RFC 8620 §3.7). A "#" key whose value is something else
			// belongs to the method: EmailSubmission/set keys its
			// onSuccessUpdateEmail map by creation reference, and "#sub"
			// points at a handle, not at a previous result (RFC 8621
			// §7.5), so it travels to the method untouched.
			refMap, ok := val.(map[string]any)
			if !ok || !isResultRef(refMap) {
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
// value (RFC 8620 §3.7). A whole-segment "*" wildcard matches every
// value at that level and the reference resolves to the array of the
// remainder applied to each — the shape `/list/*/id` relies on.
func jsonPointer(v any, path string) (any, error) {
	if path == "" {
		return v, nil
	}
	if path[0] != '/' {
		return nil, fmt.Errorf("pointer %q must start with /", path)
	}
	return resolvePointer(v, splitPointer(path[1:]))
}

func resolvePointer(cur any, toks []string) (any, error) {
	for i, tok := range toks {
		if tok == "*" {
			rest := toks[i+1:]
			switch node := cur.(type) {
			case []any:
				out := make([]any, 0, len(node))
				for _, item := range node {
					resolved, err := resolvePointer(item, rest)
					if err != nil {
						return nil, err
					}
					out = append(out, resolved)
				}
				return out, nil
			case map[string]any:
				keys := make([]string, 0, len(node))
				for k := range node {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				out := make([]any, 0, len(node))
				for _, k := range keys {
					resolved, err := resolvePointer(node[k], rest)
					if err != nil {
						return nil, err
					}
					out = append(out, resolved)
				}
				return out, nil
			default:
				return nil, fmt.Errorf("pointer %q does not resolve", joinPointer(toks))
			}
		}
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, fmt.Errorf("pointer %q does not resolve", joinPointer(toks))
			}
			cur = next
		case []any:
			idx, err := atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("pointer %q does not resolve", joinPointer(toks))
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("pointer %q does not resolve", joinPointer(toks))
		}
	}
	return cur, nil
}

func joinPointer(toks []string) string {
	return "/" + strings.Join(toks, "/")
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
	if s == "" {
		return 0, fmt.Errorf("empty index")
	}
	// Array indices never need many digits; a length bound prevents the
	// int multiply below from overflowing into a plausible index.
	if len(s) > 9 {
		return 0, fmt.Errorf("index out of range")
	}
	n := 0
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
