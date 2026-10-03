package fixturegmail

import (
	"encoding/json"
	"net/http"
	"testing"
)

func get(t *testing.T, s *Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	s := Start(t, Options{Token: "tok"})
	resp, err := http.Get(s.URL() + "gmail/v1/users/me/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProfileReportsSeed(t *testing.T) {
	s := Start(t, Options{User: "seed@example.test", Token: "tok"})
	s.SeedMessage(SeedMessage{ID: "m1", LabelIDs: []string{LabelInbox}})
	resp := get(t, s, "gmail/v1/users/me/profile")
	defer func() { _ = resp.Body.Close() }()
	var profile struct {
		EmailAddress  string `json:"emailAddress"`
		MessagesTotal int64  `json:"messagesTotal"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		t.Fatal(err)
	}
	if profile.EmailAddress != "seed@example.test" || profile.MessagesTotal != 1 {
		t.Fatalf("profile = %+v", profile)
	}
}

func TestHistoryExpiryTier(t *testing.T) {
	s := Start(t, Options{Tier: TierHistoryExpired, Token: "tok"})
	resp := get(t, s, "gmail/v1/users/me/history?startHistoryId=1")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
