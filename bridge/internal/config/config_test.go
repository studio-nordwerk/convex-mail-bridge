package config

import (
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"strings"
	"testing"
)

func load(vars map[string]string) (*Config, error) {
	return Load(func(k string) string { return vars[k] })
}

func TestStartNeedsSecretAndURL(t *testing.T) {
	secret := "a-secret-with-enough-characters-123"
	if _, err := load(map[string]string{}); err == nil || !strings.Contains(err.Error(), "MAIL_BRIDGE_SECRET") || !strings.Contains(err.Error(), "CONVEX_SITE_URL") {
		t.Fatalf("empty environment: %v", err)
	}
	if _, err := load(map[string]string{"MAIL_BRIDGE_SECRET": secret}); err == nil || !strings.Contains(err.Error(), "CONVEX_SITE_URL") {
		t.Fatalf("no URL: %v", err)
	}
	if _, err := load(map[string]string{"CONVEX_SITE_URL": "https://example.convex.site"}); err == nil {
		t.Fatal("no secret accepted")
	}
	_, err := load(map[string]string{"CONVEX_SITE_URL": "https://example.convex.site", "MAIL_BRIDGE_SECRET": "tinysecret"})
	if err == nil || strings.Contains(err.Error(), "tinysecret") {
		t.Fatalf("short secret: %v", err)
	}
	// The edge is switched off with a secret of less than 32 characters
	// (after trimming): such a bridge could never work.
	for _, s := range []string{strings.Repeat("a", 31), "  " + strings.Repeat("a", 31) + "  ", strings.Repeat("a", 24)} {
		_, err := load(map[string]string{"CONVEX_SITE_URL": "https://example.convex.site", "MAIL_BRIDGE_SECRET": s})
		if err == nil || !strings.Contains(err.Error(), "32") || strings.Contains(err.Error(), "aaaa") {
			t.Fatalf("secret of %d characters: %v", len(strings.TrimSpace(s)), err)
		}
	}
	c32, err := load(map[string]string{"CONVEX_SITE_URL": "https://example.convex.site", "MAIL_BRIDGE_SECRET": " " + strings.Repeat("a", 32) + "\n"})
	if err != nil || c32.Secret != strings.Repeat("a", 32) {
		t.Fatalf("secret of 32 characters with blanks around: %v", err)
	}
	// plain http only for this machine
	for _, u := range []string{"http://example.convex.site", "http://host.docker.internal:3211", "ftp://x.test", "https://user:pw@x.test", "not a url", "https://x.test/?a=b"} {
		_, err := load(map[string]string{"CONVEX_SITE_URL": u, "MAIL_BRIDGE_SECRET": secret})
		if err == nil && edge.PlainHTTPAnyHost && strings.HasPrefix(u, "http://") {
			continue // built with -tags localedge for the local probe: allowed there, and only there
		}
		if err == nil {
			t.Errorf("%q accepted", u)
		} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "pw@") {
			t.Errorf("%q: the error repeats a value: %v", u, err)
		}
	}
	for _, u := range []string{"https://example.convex.site", "https://example.convex.site/", "http://127.0.0.1:3211", "http://localhost:3211"} {
		c, err := load(map[string]string{"CONVEX_SITE_URL": u, "MAIL_BRIDGE_SECRET": secret})
		if err != nil {
			t.Errorf("%q refused: %v", u, err)
		} else if strings.HasSuffix(c.SiteURL, "/") {
			t.Errorf("%q: trailing slash kept", u)
		}
	}
}

func TestDefaultsAndBounds(t *testing.T) {
	base := map[string]string{"CONVEX_SITE_URL": "https://example.convex.site", "MAIL_BRIDGE_SECRET": "a-secret-with-enough-characters-123"}
	c, err := load(base)
	if err != nil {
		t.Fatal(err)
	}
	if !c.ProxyRequired {
		t.Fatal("PROXY protocol is not required by default")
	}
	if c.SMTPMaxBytes != 5<<20 || c.SMTPMaxRecipients != 50 || c.MaxAuthFailures != 3 || c.PreauthTimeout.Seconds() != 30 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.RawCacheBytes != 64<<20 || c.SeqInterval.Seconds() != 2 || c.AdminAddr != "127.0.0.1:8080" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.IMAPMaxLine >= 50*1024 {
		t.Fatal("line limit is not below the library's")
	}
	bad := map[string]string{"PROXY_PROTOCOL": "maybe", "IMAP_MAX_CONNS": "many", "SMTP_MAX_RECIPIENTS": "0", "PROXY_TRUSTED_CIDRS": "10.0.0.0/8, nonsense",
		"WORK_MEM_BYTES": "33554432"} // too small for the default mail sizes
	for k, v := range bad {
		vars := map[string]string{k: v}
		for bk, bv := range base {
			vars[bk] = bv
		}
		if _, err := load(vars); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
	vars := map[string]string{"PROXY_PROTOCOL": "off", "PROXY_TRUSTED_CIDRS": "172.16.0.0/12,fdaa::/16", "SEQ_INTERVAL_MS": "30000"}
	for bk, bv := range base {
		vars[bk] = bv
	}
	c, err = load(vars)
	if err != nil || c.ProxyRequired || len(c.TrustedPeers) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	if c.Freshness < 2*c.SeqInterval {
		t.Fatal("session confirmation is shorter than two poll intervals")
	}
}
