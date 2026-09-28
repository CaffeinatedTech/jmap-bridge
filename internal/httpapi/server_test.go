package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/fixture"
)

const testConfig = `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/jmap-bridge-test"

[[accounts]]
id = "personal"
address = "me@example.test"
token = "tok-personal"

[[accounts]]
id = "work"
address = "me@work.example.test"
token = "tok-work"
`

type testServer struct {
	*httptest.Server
	cfg *config.Config
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	cfg, err := config.LoadReader(strings.NewReader(testConfig))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	tokens := auth.NewTokens(map[string]string{
		"personal": "tok-personal",
		"work":     "tok-work",
	})
	ts := httptest.NewServer(New(cfg, tokens, fixture.New(), nil))
	t.Cleanup(ts.Close)
	return &testServer{Server: ts, cfg: cfg}
}

// do performs one request with optional Basic credentials.
func (s *testServer) do(t *testing.T, method, path, user, pass, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// postAPI sends a JMAP batch as the given user.
func (s *testServer) postAPI(t *testing.T, user, pass, body string) *http.Response {
	t.Helper()
	return s.do(t, http.MethodPost, "/personal/jmap", user, pass, "application/json", body)
}

func mustAPI(t *testing.T, s *testServer, body string, user ...string) map[string]any {
	t.Helper()
	u := "any"
	if len(user) > 0 {
		u = user[0]
	}
	resp := s.postAPI(t, u, "tok-personal", body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /jmap = %d, want 200: %s", resp.StatusCode, raw)
	}
	return decodeJSON(t, resp)
}

// responsesAsList flattens methodResponses into [name, args, callId]
// triples for assertions.
func responsesAsList(t *testing.T, resp map[string]any) [][]any {
	t.Helper()
	raw, ok := resp["methodResponses"].([]any)
	if !ok {
		t.Fatalf("methodResponses missing: %v", resp)
	}
	out := make([][]any, 0, len(raw))
	for _, entry := range raw {
		triple, ok := entry.([]any)
		if !ok || len(triple) != 3 {
			t.Fatalf("malformed invocation: %v", entry)
		}
		out = append(out, triple)
	}
	return out
}

// --- FR-J.1: session ---

func TestSessionShape(t *testing.T) {
	s := newTestServer(t)
	resp := s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "tok-personal", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	sess := decodeJSON(t, resp)

	if got, want := sess["apiUrl"], "http://127.0.0.1:8080/personal/jmap"; got != want {
		t.Errorf("apiUrl = %v, want %v", got, want)
	}
	// FR-J.5: endpoints that do not exist must not be advertised.
	for _, key := range []string{"uploadUrl", "downloadUrl", "eventSourceUrl"} {
		if _, present := sess[key]; present {
			t.Errorf("session advertises %s but the endpoint does not exist", key)
		}
	}
	if _, ok := sess["state"].(string); !ok || sess["state"] == "" {
		t.Errorf("state = %v, want a non-empty string", sess["state"])
	}
	if got, want := sess["username"], "alice"; got != want {
		t.Errorf("username = %v, want %v", got, want)
	}

	caps, _ := sess["capabilities"].(map[string]any)
	if _, ok := caps["urn:ietf:params:jmap:core"]; !ok {
		t.Error("missing core capability")
	}
	if _, ok := caps["urn:ietf:params:jmap:mail"]; !ok {
		t.Error("missing mail capability")
	}
	// Honest capabilities: nothing is configured or implemented beyond
	// core+mail at M0 (FR-J.5).
	if _, ok := caps["urn:ietf:params:jmap:submission"]; ok {
		t.Error("submission capability advertised before SMTP submission exists")
	}
	if _, ok := caps["urn:ietf:params:jmap:contacts"]; ok {
		t.Error("contacts capability advertised without CardDAV")
	}
	mail, ok := caps["urn:ietf:params:jmap:mail"].(map[string]any)
	if !ok || len(mail) != 0 {
		t.Errorf("session-level mail capability must be {}, got %v", caps["urn:ietf:params:jmap:mail"])
	}

	primary, _ := sess["primaryAccounts"].(map[string]any)
	if primary["urn:ietf:params:jmap:mail"] != "personal" {
		t.Errorf("primaryAccounts = %v", primary)
	}
	accounts, _ := sess["accounts"].(map[string]any)
	pa, ok := accounts["personal"].(map[string]any)
	if !ok {
		t.Fatalf("accounts = %v", accounts)
	}
	if pa["name"] != "me@example.test" || pa["isPersonal"] != true {
		t.Errorf("personal account = %v", pa)
	}
	acctCaps, _ := pa["accountCapabilities"].(map[string]any)
	mailCap, ok := acctCaps["urn:ietf:params:jmap:mail"].(map[string]any)
	if !ok {
		t.Fatalf("accountCapabilities = %v", acctCaps)
	}
	if mailCap["mayCreateTopLevelMailbox"] != false {
		t.Errorf("mayCreateTopLevelMailbox = %v, want false until Mailbox/set exists", mailCap["mayCreateTopLevelMailbox"])
	}
	if got := mailCap["emailQuerySortOptions"]; got == nil {
		t.Error("emailQuerySortOptions missing")
	}
}

func TestSessionRejectsBadAuth(t *testing.T) {
	s := newTestServer(t)
	cases := []struct{ name, user, pass string }{
		{"missing header", "", ""},
		{"wrong token", "alice", "nope"},
		{"empty username", "", "tok-personal"},
		{"other account's token", "alice", "tok-work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.do(t, http.MethodGet, "/personal/.well-known/jmap", tc.user, tc.pass, "", "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate (FR-J.6)")
			}
		})
	}
}

func TestUnknownAccountIsIndistinguishable(t *testing.T) {
	s := newTestServer(t)
	// A valid personal token against a real other account and against a
	// nonexistent one must answer identically (FR-A.11).
	existing := s.do(t, http.MethodGet, "/work/.well-known/jmap", "alice", "tok-personal", "", "")
	missing := s.do(t, http.MethodGet, "/ghost/.well-known/jmap", "alice", "tok-personal", "", "")
	if existing.StatusCode != http.StatusUnauthorized || missing.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 and 401 (FR-A.11)", existing.StatusCode, missing.StatusCode)
	}
}

func TestWrongMethodAndUnknownPaths(t *testing.T) {
	s := newTestServer(t)
	if resp := s.do(t, http.MethodPost, "/personal/.well-known/jmap", "a", "tok-personal", "application/json", "{}"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST session = %d, want 405", resp.StatusCode)
	}
	if resp := s.do(t, http.MethodGet, "/personal/jmap", "a", "tok-personal", "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET jmap = %d, want 405", resp.StatusCode)
	}
	// /{account}/jmap also claims the unprefixed /.well-known/jmap path
	// (account would be ".well-known"); GET there is a method mismatch.
	if resp := s.do(t, http.MethodGet, "/.well-known/jmap", "a", "tok-personal", "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("root session = %d, want 405", resp.StatusCode)
	}
	if resp := s.do(t, http.MethodGet, "/personal/other", "a", "tok-personal", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", resp.StatusCode)
	}
}

// --- FR-J.2/FR-J.3: dispatch and references ---

func TestUnknownMethodIsErrorInvocationNot500(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Vacation/set", {}, "c1"]]
	}`)
	entries := responsesAsList(t, resp)
	if len(entries) != 1 || entries[0][0] != "error" {
		t.Fatalf("responses = %v", entries)
	}
	args := entries[0][1].(map[string]any)
	if args["type"] != "unknownMethod" {
		t.Errorf("error type = %v, want unknownMethod", args["type"])
	}
	if entries[0][2] != "c1" {
		t.Errorf("call id = %v, want c1", entries[0][2])
	}
}

func TestResultReferenceResolution(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [
			["Mailbox/query", {"accountId":"personal","sort":[{"property":"sortOrder","isAscending":true}],"calculateTotal":true}, "q1"],
			["Mailbox/get", {"accountId":"personal","#ids":{"resultOf":"q1","name":"Mailbox/query","path":"/ids"}}, "g1"]
		]
	}`)
	entries := responsesAsList(t, resp)
	if len(entries) != 2 {
		t.Fatalf("responses = %v", entries)
	}
	if entries[0][0] != "Mailbox/query" || entries[1][0] != "Mailbox/get" {
		t.Fatalf("method names = %v, %v", entries[0][0], entries[1][0])
	}
	query := entries[0][1].(map[string]any)
	get := entries[1][1].(map[string]any)
	qIDs := query["ids"].([]any)
	list := get["list"].([]any)
	if len(list) != len(qIDs) {
		t.Fatalf("get returned %d mailboxes for %d query ids", len(list), len(qIDs))
	}
	// Order must follow the query's ids (FR-J.3).
	for i, entry := range list {
		if entry.(map[string]any)["id"] != qIDs[i] {
			t.Fatalf("list[%d] id = %v, want %v", i, entry.(map[string]any)["id"], qIDs[i])
		}
	}
}

func TestResultReferenceUnknownTarget(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [
			["Mailbox/get", {"accountId":"personal","#ids":{"resultOf":"ghost","name":"Mailbox/query","path":"/ids"}}, "g1"]
		]
	}`)
	entries := responsesAsList(t, resp)
	args := entries[0][1].(map[string]any)
	if entries[0][0] != "error" || args["type"] != "invalidResultReference" {
		t.Fatalf("responses = %v", entries)
	}
}

// --- request-level problems (FR-J.2, NFR-5) ---

func TestRequestLevelProblems(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantType    string
	}{
		{"not JSON content type", "text/plain", `{"using":[],"methodCalls":[]}`, 400, "notJSON"},
		{"invalid JSON", "application/json", "{nope", 400, "notJSON"},
		{"not a request", "application/json", `{"using":[]}`, 400, "notRequest"},
		{
			"unknown capability", "application/json",
			`{"using":["urn:ietf:params:jmap:contacts"],"methodCalls":[]}`, 400, "unknownCapability",
		},
		{"too many calls", "application/json", overLimitBody(), 400, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.do(t, http.MethodPost, "/personal/jmap", "any", "tok-personal", tc.contentType, tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			prob := decodeJSON(t, resp)
			typ, _ := prob["type"].(string)
			if !strings.HasSuffix(typ, ":"+tc.wantType) {
				t.Errorf("problem type = %q, want suffix %q", typ, tc.wantType)
			}
		})
	}
}

func overLimitBody() string {
	b := &strings.Builder{}
	b.WriteString(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[`)
	for i := 0; i < 257; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(b, `["Mailbox/get",{"accountId":"personal"},"c%d"]`, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestOversizedRequestRejected(t *testing.T) {
	s := newTestServer(t)
	pad := strings.Repeat("x", maxSizeRequest+1024)
	body := `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[],"pad":"` + pad + `"}`
	resp := s.postAPI(t, "any", "tok-personal", body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	prob := decodeJSON(t, resp)
	if prob["limit"] != "maxSizeRequest" {
		t.Errorf("limit = %v, want maxSizeRequest", prob["limit"])
	}
}

// --- method-level reads ---

func TestEmailQueryGetPropertyAndBodies(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [
			["Email/query", {"accountId":"personal","filter":{"inMailbox":"mb-inbox"},"calculateTotal":true}, "q1"],
			["Email/get", {
				"accountId":"personal",
				"#ids": {"resultOf":"q1","name":"Email/query","path":"/ids"},
				"properties": ["threadId","mailboxIds","keywords","from","to","subject","receivedAt","size","hasAttachment","preview"]
			}, "g1"],
			["Email/get", {
				"accountId":"personal",
				"#ids": {"resultOf":"q1","name":"Email/query","path":"/ids"},
				"properties": ["subject","textBody","htmlBody","bodyValues","preview"],
				"bodyProperties": ["partId","type","charset","size"],
				"fetchAllBodyValues": true
			}, "g2"]
		]
	}`)
	entries := responsesAsList(t, resp)
	if len(entries) != 3 {
		t.Fatalf("responses = %v", entries)
	}
	query := entries[0][1].(map[string]any)
	if query["total"] != 5.0 {
		t.Errorf("inbox total = %v, want 5", query["total"])
	}

	summary := entries[1][1].(map[string]any)["list"].([]any)
	first := summary[0].(map[string]any)
	if _, ok := first["bodyValues"]; ok {
		t.Error("summary get returned bodyValues without a fetch flag")
	}
	if _, ok := first["textBody"]; ok {
		t.Error("summary get returned textBody outside the requested properties")
	}
	for _, key := range []string{"subject", "preview", "receivedAt", "mailboxIds"} {
		if _, ok := first[key]; !ok {
			t.Errorf("summary get missing requested property %q", key)
		}
	}

	bodyGet := entries[2][1].(map[string]any)["list"].([]any)
	body := bodyGet[0].(map[string]any)
	values := body["bodyValues"].(map[string]any)
	if len(values) == 0 {
		t.Fatal("fetchAllBodyValues returned no bodyValues")
	}
	part := values["1"].(map[string]any)
	if part["value"] == "" {
		t.Error("bodyValues[1].value is empty")
	}
	textParts := body["textBody"].([]any)
	partObj := textParts[0].(map[string]any)
	if _, ok := partObj["charset"]; !ok {
		t.Errorf("bodyProperties not honoured: %v", partObj)
	}
	if _, ok := partObj["isTruncated"]; ok {
		t.Error("unexpected part property outside bodyProperties")
	}
}

func TestEmailGetUnknownIDs(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Email/get", {"accountId":"personal","ids":["nope","gone"]}, "g1"]]
	}`)
	get := responsesAsList(t, resp)[0][1].(map[string]any)
	notFound := get["notFound"].([]any)
	if len(notFound) != 2 {
		t.Errorf("notFound = %v, want both ids", notFound)
	}
	if list := get["list"].([]any); len(list) != 0 {
		t.Errorf("list = %v, want empty", list)
	}
}

func TestWrongAccountIDFailsClosed(t *testing.T) {
	s := newTestServer(t)
	for _, id := range []string{"work", "ghost"} {
		resp := mustAPI(t, s, fmt.Sprintf(
			`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
			  "methodCalls":[["Mailbox/get",{"accountId":"%s","ids":["mb-inbox"]},"g1"]]}`, id))
		entries := responsesAsList(t, resp)
		if entries[0][0] != "error" {
			t.Fatalf("accountId %q: responses = %v", id, entries)
		}
		if args := entries[0][1].(map[string]any); args["type"] != "notFound" {
			t.Errorf("accountId %q: type = %v, want notFound", id, args["type"])
		}
	}
}

func TestMissingAccountIDIsInvalidArguments(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Mailbox/get", {"ids":["mb-inbox"]}, "g1"]]
	}`)
	entries := responsesAsList(t, resp)
	if entries[0][0] != "error" {
		t.Fatalf("responses = %v", entries)
	}
	if args := entries[0][1].(map[string]any); args["type"] != "invalidArguments" {
		t.Errorf("type = %v, want invalidArguments", args["type"])
	}
}

func TestUnsupportedEmailSortProperty(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Email/query", {"accountId":"personal","sort":[{"property":"bogus"}]}, "q1"]]
	}`)
	entries := responsesAsList(t, resp)
	args := entries[0][1].(map[string]any)
	if entries[0][0] != "error" || args["type"] != "invalidArguments" {
		t.Fatalf("responses = %v", entries)
	}
}

func TestThreadGet(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Thread/get", {"accountId":"personal","ids":["thr-report","ghost"]}, "t1"]]
	}`)
	get := responsesAsList(t, resp)[0][1].(map[string]any)
	list := get["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	thread := list[0].(map[string]any)
	ids := thread["emailIds"].([]any)
	if len(ids) != 2 || ids[0] != "em-report" || ids[1] != "em-report-reply" {
		t.Errorf("emailIds = %v, want oldest-first thread pair", ids)
	}
	notFound := get["notFound"].([]any)
	if len(notFound) != 1 || notFound[0] != "ghost" {
		t.Errorf("notFound = %v", notFound)
	}
}

func TestChanges(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [
			["Email/changes", {"accountId":"personal","sinceState":"1"}, "c1"],
			["Mailbox/changes", {"accountId":"personal","sinceState":"1"}, "c2"]
		]
	}`)
	for _, entry := range responsesAsList(t, resp) {
		args := entry[1].(map[string]any)
		if args["newState"] != "1" {
			t.Errorf("%v newState = %v", entry[0], args["newState"])
		}
		if len(args["updated"].([]any)) != 0 {
			t.Errorf("%v updated = %v, want empty", entry[0], args["updated"])
		}
	}
}

func TestChangesUnknownState(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Email/changes", {"accountId":"personal","sinceState":""}, "c1"]]
	}`)
	entries := responsesAsList(t, resp)
	if entries[0][0] != "error" {
		t.Fatalf("responses = %v", entries)
	}
	if args := entries[0][1].(map[string]any); args["type"] != "cannotCalculateChanges" {
		t.Errorf("type = %v, want cannotCalculateChanges", args["type"])
	}
}

func TestSessionStateEchoedOnAPIResponses(t *testing.T) {
	s := newTestServer(t)
	// Same credentials on both endpoints: the state string must match
	// (RFC 8620 §2), so a client can compare the API's sessionState with
	// the session it fetched.
	sess := decodeJSON(t, s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "tok-personal", "", ""))
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [["Mailbox/get", {"accountId":"personal","ids":[]}, "g1"]]
	}`, "alice")
	if resp["sessionState"] != sess["state"] {
		t.Errorf("API sessionState = %v, session state = %v", resp["sessionState"], sess["state"])
	}
}

func TestAuthNoneServesWithoutCredentials(t *testing.T) {
	cfg, err := config.LoadReader(strings.NewReader(`
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/jmap-bridge-test"

[auth]
mode = "none"

[[accounts]]
id = "personal"
address = "me@example.test"
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	ts := httptest.NewServer(New(cfg, auth.NewTokens(nil), fixture.New(), nil))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/personal/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session without credentials = %d, want 200 in none mode", resp.StatusCode)
	}

	// An unknown account is a plain 404 in none mode: there is no
	// credential to mask it with.
	resp2, err := http.Get(ts.URL + "/ghost/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown account in none mode = %d, want 404", resp2.StatusCode)
	}
}

func TestBatchedMethodsProcessedInOrder(t *testing.T) {
	s := newTestServer(t)
	resp := mustAPI(t, s, `{
		"using": ["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls": [
			["Email/get", {"accountId":"personal","ids":["em-welcome"],"properties":["subject"]}, "a"],
			["Nope/nope", {}, "b"],
			["Mailbox/query", {"accountId":"personal"}, "c"]
		]
	}`)
	entries := responsesAsList(t, resp)
	if len(entries) != 3 {
		t.Fatalf("responses = %v", entries)
	}
	want := []string{"Email/get", "error", "Mailbox/query"}
	for i, name := range want {
		if entries[i][0] != name {
			t.Errorf("entry %d = %v, want %v", i, entries[i][0], name)
		}
	}
	if entries[1][2] != "b" {
		t.Errorf("error call id = %v", entries[1][2])
	}
}
