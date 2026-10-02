package ratelimit

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLockoutAfterFailures(t *testing.T) {
	l := New(Config{AuthFailures: 3, AuthWindow: time.Minute, AuthBlock: time.Minute})
	key := "auth:203.0.113.5:personal"
	for i := 0; i < 2; i++ {
		l.Fail(key)
		if l.Blocked(key) {
			t.Fatalf("blocked after %d failures, want 3", i+1)
		}
	}
	l.Fail(key)
	if !l.Blocked(key) {
		t.Fatal("not blocked at the third failure")
	}
}

func TestSucceedClearsFailures(t *testing.T) {
	l := New(Config{AuthFailures: 3, AuthWindow: time.Minute, AuthBlock: time.Minute})
	key := "k"
	l.Fail(key)
	l.Fail(key)
	l.Succeed(key)
	l.Fail(key)
	l.Fail(key)
	if l.Blocked(key) {
		t.Fatal("a success must reset the failure count")
	}
}

func TestFailuresOutsideWindowDoNotAccumulate(t *testing.T) {
	l := New(Config{AuthFailures: 3, AuthWindow: 40 * time.Millisecond, AuthBlock: time.Minute})
	key := "k"
	l.Fail(key)
	l.Fail(key)
	time.Sleep(60 * time.Millisecond)
	l.Fail(key) // new window: count resets to 1
	if l.Blocked(key) {
		t.Fatal("failures from an expired window must not count")
	}
}

func TestBlockExpires(t *testing.T) {
	l := New(Config{AuthFailures: 1, AuthWindow: time.Minute, AuthBlock: 40 * time.Millisecond})
	key := "k"
	l.Fail(key)
	if !l.Blocked(key) {
		t.Fatal("not blocked")
	}
	time.Sleep(60 * time.Millisecond)
	if l.Blocked(key) {
		t.Fatal("still blocked after the block window elapsed")
	}
}

func TestKeysAreBounded(t *testing.T) {
	l := New(Config{AuthFailures: 1, AuthWindow: time.Minute, AuthBlock: time.Minute, MaxKeys: 50})
	for i := 0; i < 500; i++ {
		l.Fail(keyN(i))
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > 50 {
		t.Fatalf("entries = %d, want <= MaxKeys(50)", n)
	}
}

func keyN(i int) string {
	return "auth:198.51.100." + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

func TestClientIP(t *testing.T) {
	ipHeader := Config{
		TrustedProxies: []string{"10.0.0.0/8"},
		ClientIPHeader: "CF-Connecting-IP",
	}
	xffOnly := Config{TrustedProxies: []string{"10.0.0.0/8"}}
	untrusted := Config{TrustedProxies: []string{"10.0.0.0/8"}}

	cases := []struct {
		name   string
		cfg    Config
		remote string
		header map[string]string
		want   string
	}{
		{
			name:   "no trusted proxies ignores forwarded headers",
			cfg:    Config{},
			remote: "198.51.100.7:1234",
			header: map[string]string{"X-Forwarded-For": "203.0.113.9", "CF-Connecting-IP": "203.0.113.9"},
			want:   "198.51.100.7",
		},
		{
			name:   "trusted peer uses CF-Connecting-IP",
			cfg:    ipHeader,
			remote: "10.0.0.9:1234",
			header: map[string]string{"CF-Connecting-IP": "203.0.113.5"},
			want:   "203.0.113.5",
		},
		{
			name:   "trusted peer walks XFF from the right",
			cfg:    xffOnly,
			remote: "10.0.0.9:1234",
			header: map[string]string{"X-Forwarded-For": "forged, 203.0.113.5, 10.0.0.2"},
			want:   "203.0.113.5",
		},
		{
			name:   "untrusted peer cannot spoof",
			cfg:    untrusted,
			remote: "198.51.100.7:1234",
			header: map[string]string{"X-Forwarded-For": "203.0.113.9"},
			want:   "198.51.100.7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New(tc.cfg)
			r := httptest.NewRequest("GET", "http://example.test/", nil)
			r.RemoteAddr = tc.remote
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			if got := l.ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
