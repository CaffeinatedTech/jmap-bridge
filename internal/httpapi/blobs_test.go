package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// zeroReader supplies endless NUL bytes: enough to walk an upload past
// its cap without a test holding the payload in memory.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

// submitConfig is an account with IMAP *and* SMTP behind it — the shape
// FR-J.5/FR-M.14 gate the submission capability and identity on. The
// addresses are loopback placeholders: nothing here dials anywhere.
const submitConfig = `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/jmap-bridge-test"

[[accounts]]
id = "personal"
name = "Personal"
address = "me@example.test"
token = "tok-personal"

  [accounts.imap]
  host = "127.0.0.1"
  port = 143
  tls = false
  username = "me@example.test"
  password = "pw"

  [accounts.smtp]
  host = "127.0.0.1"
  port = 2525
  tls = "none"
  username = "me@example.test"
  password = "pw"
`

// --- FR-M.16 / FR-M.17: blob upload and download ---

func TestUploadDownloadRoundTrip(t *testing.T) {
	s := newTestServer(t)
	payload := []byte("attachment payload\x00\xff\r\n.end\r\n")

	resp := s.do(t, http.MethodPost, "/personal/upload/", "alice", "tok-personal",
		"application/octet-stream", string(payload))
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d: %s", resp.StatusCode, raw)
	}
	up := decodeJSON(t, resp)
	if up["accountId"] != "personal" || up["type"] != "application/octet-stream" {
		t.Errorf("upload response = %v", up)
	}
	if size, _ := up["size"].(float64); int(size) != len(payload) {
		t.Errorf("size = %v, want %d", up["size"], len(payload))
	}
	blobID, _ := up["blobId"].(string)
	if blobID == "" {
		t.Fatalf("upload returned no blobId: %v", up)
	}

	// Download through the session's own template, expanded exactly the
	// way a client does it (RFC 8620 §6.2).
	sess := decodeJSON(t, s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "tok-personal", "", ""))
	dl, _ := sess["downloadUrl"].(string)
	path := strings.NewReplacer(
		"{accountId}", "personal",
		"{blobId}", blobID,
		"{name}", "notes.bin",
		"{type}", "application/octet-stream",
	).Replace(strings.TrimPrefix(dl, "http://127.0.0.1:8080"))
	got := s.do(t, http.MethodGet, path, "alice", "tok-personal", "", "")
	if got.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(got.Body)
		t.Fatalf("download = %d: %s", got.StatusCode, raw)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("round trip bytes = %q, want %q", body, payload)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want what {type} asked for", ct)
	}
	if cd := got.Header.Get("Content-Disposition"); !strings.Contains(cd, "notes.bin") {
		t.Errorf("Content-Disposition = %q, want the {name} filename", cd)
	}
	if cc := got.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want the immutable caching RFC 8620 §6.2 recommends", cc)
	}
}

func TestUploadRequiresAuthAndRejectsBadContent(t *testing.T) {
	s := newTestServer(t)

	t.Run("no credentials", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/personal/upload/", "", "", "application/octet-stream", "x")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (FR-J.6)", resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Error("401 without WWW-Authenticate")
		}
	})

	t.Run("unparseable media type", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/personal/upload/", "alice", "tok-personal",
			"not a media type", "x")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		if body := decodeJSON(t, resp); body["type"] != "urn:ietf:params:jmap:error:invalidArguments" {
			t.Errorf("problem = %v", body)
		}
	})

	// The fast path: a client that declares an oversize body is refused
	// before a byte of it is read (served straight to the handler — an
	// HTTP client refuses to lie about Content-Length).
	t.Run("declared oversize", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/personal/upload/", strings.NewReader("x"))
		req.SetBasicAuth("alice", "tok-personal")
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = maxSizeUpload + 1
		rec := s.serve(t, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "urn:ietf:params:jmap:error:limit" || body["limit"] != "maxSizeUpload" {
			t.Errorf("problem = %v, want the limit problem naming maxSizeUpload (RFC 8620 §3.6.1)", body)
		}
	})

	// The slow path: a chunked body that runs past the cap is cut off
	// and refused (NFR-5, FR-M.16).
	t.Run("streamed oversize", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, s.URL+"/personal/upload/", io.LimitReader(zeroReader{}, maxSizeUpload+1))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("alice", "tok-personal")
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("stream oversize: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
		if body := decodeJSON(t, resp); body["type"] != "urn:ietf:params:jmap:error:limit" {
			t.Errorf("problem = %v, want the limit problem", body)
		}
	})

	t.Run("no type at all", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/personal/upload/", "alice", "tok-personal", "", "x")
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("upload without Content-Type = %d: %s", resp.StatusCode, raw)
		}
		if got := decodeJSON(t, resp)["type"]; got != "application/octet-stream" {
			t.Errorf("type = %v, want the octet-stream default", got)
		}
	})
}

func TestDownloadUnknownAndForeignBlobsAre404(t *testing.T) {
	s := newTestServer(t)

	unknown := s.do(t, http.MethodGet, "/personal/download/nope/notes.bin?type=text/plain",
		"alice", "tok-personal", "", "")
	if unknown.StatusCode != http.StatusNotFound {
		t.Errorf("unknown blob = %d, want 404", unknown.StatusCode)
	}
	if body := decodeJSON(t, unknown); body["type"] != "urn:ietf:params:jmap:error:notFound" {
		t.Errorf("problem = %v", body)
	}

	// A blob that exists in another account answers exactly the same:
	// a probe learns nothing about accounts it is not authenticated for
	// (FR-M.17, FR-A.11).
	foreign := s.do(t, http.MethodPost, "/personal/upload/", "alice", "tok-personal",
		"application/octet-stream", "personal bytes")
	blobID, _ := decodeJSON(t, foreign)["blobId"].(string)
	viaWork := s.do(t, http.MethodGet, "/work/download/"+blobID+"/x.bin?type=text/plain",
		"bob", "tok-work", "", "")
	if viaWork.StatusCode != http.StatusNotFound {
		t.Errorf("foreign blob via another account = %d, want 404", viaWork.StatusCode)
	}
}

func TestDownloadRefusesATypeItCannotSend(t *testing.T) {
	s := newTestServer(t)
	up := decodeJSON(t, s.do(t, http.MethodPost, "/personal/upload/", "alice", "tok-personal",
		"text/plain", "hello"))
	blobID, _ := up["blobId"].(string)

	// A {type} carrying a header injection must not be echoed into the
	// response headers (NFR-5); the recorded type stands in.
	resp := s.do(t, http.MethodGet,
		"/personal/download/"+blobID+"/x.bin?type=text%2Fplain%3B%0D%0AX-Injected%3A%20yes",
		"alice", "tok-personal", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200 with a safe type", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); strings.ContainsAny(got, "\r\n") {
		t.Errorf("Content-Type = %q contains a header break", got)
	}
	if resp.Header.Get("X-Injected") != "" {
		t.Error("the injected header made it into the response")
	}
	if got := resp.Header.Get("Content-Disposition"); strings.ContainsAny(got, "\r\n") {
		t.Errorf("Content-Disposition = %q contains a header break", got)
	}
}

// --- FR-J.5 / FR-M.14: submission is advertised only when configured ---

func TestSessionAdvertisesSubmissionWithSMTP(t *testing.T) {
	s := newTestServerCfg(t, submitConfig)
	sess := decodeJSON(t, s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "tok-personal", "", ""))

	caps, _ := sess["capabilities"].(map[string]any)
	sub, ok := caps["urn:ietf:params:jmap:submission"].(map[string]any)
	if !ok {
		t.Fatalf("session capabilities = %v, want submission (an account with SMTP configured)", caps)
	}
	if len(sub) != 0 {
		t.Errorf("session-level submission = %v, want {} (RFC 8621 §1.3.2)", sub)
	}
	accounts, _ := sess["accounts"].(map[string]any)
	pa, _ := accounts["personal"].(map[string]any)
	acctCaps, _ := pa["accountCapabilities"].(map[string]any)
	acctSub, ok := acctCaps["urn:ietf:params:jmap:submission"].(map[string]any)
	if !ok {
		t.Fatalf("accountCapabilities = %v, want submission", acctCaps)
	}
	if delayed, _ := acctSub["maxDelayedSend"].(float64); delayed != 0 {
		t.Errorf("maxDelayedSend = %v, want 0 (no delayed send in v0.1)", acctSub["maxDelayedSend"])
	}
	for _, key := range []string{"uploadUrl", "downloadUrl"} {
		if _, ok := sess[key]; !ok {
			t.Errorf("session is missing %s", key)
		}
	}

	// Identity/get answers for that account — one identity, mayDelete
	// false (FR-M.14).
	res := mustAPI(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],
		"methodCalls":[["Identity/get",{"accountId":"personal"},"c1"]]}`)
	responses := responsesAsList(t, res)
	if len(responses) != 1 || responses[0][0] != "Identity/get" {
		t.Fatalf("responses = %v", responses)
	}
	raw, _ := json.Marshal(responses[0][1])
	var id struct {
		List []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Email     string `json:"email"`
			MayDelete bool   `json:"mayDelete"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	if len(id.List) != 1 || id.List[0].Email != "me@example.test" || id.List[0].Name != "Personal" {
		t.Fatalf("identity = %+v", id.List)
	}
	if id.List[0].MayDelete {
		t.Error("mayDelete = true, want false")
	}
}
