package config

import (
	"strings"
	"testing"
)

func TestRateDefaults(t *testing.T) {
	cfg := mustLoad(t, minimalLoopback)
	if !cfg.Rate.Enabled {
		t.Error("rate limiting disabled by default")
	}
	if cfg.Rate.AuthFailures != 10 ||
		cfg.Rate.MaxConcurrentRequests != 8 ||
		cfg.Rate.MaxConcurrentUploads != 4 {
		t.Errorf("rate defaults = %+v", cfg.Rate)
	}
}

// withRate inserts a [rate] block before the account table.
func withRate(body string) string {
	return strings.Replace(minimalLoopback, "[[accounts]]", "[rate]\n"+body+"\n\n[[accounts]]", 1)
}

func TestRateOverride(t *testing.T) {
	toml := withRate(`
auth_failures = 3
max_concurrent_requests = 2
trusted_proxies = ["10.0.0.0/8"]
client_ip_header = "CF-Connecting-IP"`)
	cfg := mustLoad(t, toml)
	if cfg.Rate.AuthFailures != 3 || cfg.Rate.MaxConcurrentRequests != 2 ||
		cfg.Rate.ClientIPHeader != "CF-Connecting-IP" {
		t.Errorf("rate override = %+v", cfg.Rate)
	}
}

func TestTrustedProxyMustBeCIDR(t *testing.T) {
	wantErrKey(t, withRate(`trusted_proxies = ["not-a-cidr"]`), "rate.trusted_proxies[0]")
}

func TestShortTokenRejected(t *testing.T) {
	toml := strings.Replace(minimalLoopback,
		`token = "t0ken-0123456789abcdef0123456789"`, `token = "short"`, 1)
	wantErrKey(t, toml, "accounts[0].token")
}
