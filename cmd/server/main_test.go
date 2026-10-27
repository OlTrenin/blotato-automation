package main

import "testing"

func TestConfigurationRejectsUnsafeOrInvalidSettings(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://demo:demo@localhost/demo", "API_TOKEN": "local-test-token-at-least-16"}
	for _, tc := range []struct{ name, key, value string }{
		{"missing database", "DATABASE_URL", ""}, {"short token", "API_TOKEN", "short"}, {"whitespace token", "API_TOKEN", "has whitespace in token"},
		{"invalid tenant", "TENANT_ID", "../tenant"}, {"invalid demo switch", "DEMO_MODE", "yes"}, {"zero workers", "WORKERS", "0"}, {"too many workers", "WORKERS", "33"},
		{"invalid expiry", "WAIT_TTL", "nonsense"}, {"negative expiry", "WAIT_TTL", "-1h"}, {"unbounded expiry", "WAIT_TTL", "10000h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			env[tc.key] = tc.value
			if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestConfigurationDefaultsAreExplicit(t *testing.T) {
	env := map[string]string{"DATABASE_URL": "postgres://demo:demo@localhost/demo", "API_TOKEN": "local-test-token-at-least-16"}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.demo || c.workers != 2 || c.tenantID != "tenant-demo" || c.accountID != "account-123" || c.addr != "127.0.0.1:8080" {
		t.Fatalf("unexpected defaults: %#v", c)
	}
}
