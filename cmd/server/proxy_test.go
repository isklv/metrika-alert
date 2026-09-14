package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/isklv/metrika-alert/internal/config"
)

// A proxy that is configured but silently not applied is the worst outcome:
// the client connects straight to a blocked host and reports a timeout that
// looks like a network fault rather than a configuration one.
func TestProxyTransportRejectsUnusableValues(t *testing.T) {
	tests := []struct {
		name     string
		proxyURL string
	}{
		// url.Parse reads the host as the scheme here and reports no error.
		{"no scheme", "proxy.example.com:1080"},
		{"bare host", "proxy.example.com"},
		// curl accepts socks5h; net/http's Transport does not.
		{"socks5h", "socks5h://proxy.example.com:1080"},
		{"unsupported scheme", "ftp://proxy.example.com:21"},
		{"scheme without host", "socks5://"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport, err := proxyTransport("telegram", tc.proxyURL)
			if err == nil {
				t.Fatalf("accepted %q — the client would connect directly while the config names a proxy", tc.proxyURL)
			}
			if transport != nil {
				t.Error("a rejected proxy must not yield a transport")
			}
			if !strings.Contains(err.Error(), tc.proxyURL) {
				t.Errorf("error should quote the offending value: %v", err)
			}
		})
	}
}

func TestProxyTransportAcceptsSupportedSchemes(t *testing.T) {
	for _, proxyURL := range []string{
		"http://proxy.example.com:3128",
		"https://proxy.example.com:3128",
		"socks5://proxy.example.com:1080",
		"socks5://user:pass@proxy.example.com:1080",
	} {
		transport, err := proxyTransport("telegram", proxyURL)
		if err != nil {
			t.Errorf("proxyTransport(%q): %v", proxyURL, err)
			continue
		}
		if transport == nil {
			t.Errorf("proxyTransport(%q) returned no transport", proxyURL)
			continue
		}

		// The transport must actually route through the configured proxy.
		tr, ok := transport.(*http.Transport)
		if !ok || tr.Proxy == nil {
			t.Errorf("proxyTransport(%q) built a transport with no proxy function", proxyURL)
			continue
		}
		req, _ := http.NewRequest(http.MethodGet, "https://api.telegram.org/bot/getMe", nil)
		via, err := tr.Proxy(req)
		if err != nil || via == nil {
			t.Errorf("proxyTransport(%q): request would not be proxied (%v)", proxyURL, err)
			continue
		}
		if via.Host != "proxy.example.com:3128" && via.Host != "proxy.example.com:1080" {
			t.Errorf("proxyTransport(%q) routes to %q", proxyURL, via.Host)
		}
	}
}

// No proxy configured is a legitimate setup, not an error.
func TestProxyTransportAllowsDirectConnection(t *testing.T) {
	transport, err := proxyTransport("vkteams", "")
	if err != nil {
		t.Fatalf("empty proxy_url: %v", err)
	}
	if transport != nil {
		t.Error("an empty proxy_url must yield a direct connection")
	}
}

// Credentials in a proxy URL must not reach the log.
func TestProxyCredentialsAreNotLogged(t *testing.T) {
	const secret = "hunter2"
	if _, err := proxyTransport("telegram", "socks5://user:"+secret+"@proxy.example.com:1080"); err != nil {
		t.Fatalf("proxyTransport: %v", err)
	}
	// url.Redacted is what keeps the password out; assert the helper we rely on.
	if strings.Contains(redactedFor(t, "socks5://user:"+secret+"@proxy.example.com:1080"), secret) {
		t.Error("the proxy password would be written to the log")
	}
}

func redactedFor(t *testing.T, raw string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return req.URL.Redacted()
}

// A proxy URL routinely carries a password, and this hint is printed on every
// failed connection — the most common line to end up pasted into a ticket.
func TestTelegramHintRedactsCredentials(t *testing.T) {
	const secret = "hunter2"
	cfg := &config.Config{}
	cfg.Telegram.ProxyURL = "socks5://user:" + secret + "@proxy.example.com:1080"

	hint := telegramHint(cfg)

	if strings.Contains(hint, secret) {
		t.Errorf("the proxy password leaks into the log: %s", hint)
	}
	if !strings.Contains(hint, "proxy.example.com") {
		t.Errorf("the hint should still name the proxy host: %s", hint)
	}
}

func TestTelegramHintWithoutProxy(t *testing.T) {
	hint := telegramHint(&config.Config{})
	if !strings.Contains(hint, "proxy_url") {
		t.Errorf("a direct-connection failure should suggest the proxy setting: %s", hint)
	}
}

func TestRedactProxyHandlesJunk(t *testing.T) {
	if got := redactProxy("://:::"); strings.Contains(got, ":::") {
		t.Errorf("redactProxy leaked an unparsable value: %q", got)
	}
}
