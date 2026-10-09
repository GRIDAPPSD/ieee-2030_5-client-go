package inverter

import (
	"strings"
	"testing"
)

// A PUT target is the configured server or an error, whatever shape the href
// takes; string-joining a relative href let userinfo or a host suffix move
// the request to another host.
func TestPutTarget_StaysOnTheConfiguredServer(t *testing.T) {
	cases := []struct {
		name, base, href string
		want             string // empty: refused
	}{
		{"rooted path", "https://127.0.0.1:8443", "/edev/1/frq/5", "https://127.0.0.1:8443/edev/1/frq/5"},
		{"path without a leading slash", "https://127.0.0.1:8443", "edev/1/frq/5", "https://127.0.0.1:8443/edev/1/frq/5"},
		{"absolute on the server", "https://127.0.0.1:8443", "https://127.0.0.1:8443/edev/1/frq/5", "https://127.0.0.1:8443/edev/1/frq/5"},
		{"userinfo form", "https://127.0.0.1:8443", "@evil.example/edev/1/frq/5", "https://127.0.0.1:8443/@evil.example/edev/1/frq/5"},
		{"host suffix form", "https://server.example", ".evil.example/x", "https://server.example/.evil.example/x"},
		{"scheme-relative", "https://server.example", "//evil.example/edev/1/frq/5", ""},
		{"another host", "https://server.example", "https://evil.example/edev/1/frq/5", ""},
		{"another scheme", "https://server.example", "http://server.example/edev/1/frq/5", ""},
		{"another port", "https://server.example:8443", "https://server.example:9443/x", ""},
		{"default port written out", "https://h:443", "https://h/edev/1/frq/5", "https://h/edev/1/frq/5"},
		{"default port omitted from the base", "https://h", "https://h:443/edev/1/frq/5", "https://h:443/edev/1/frq/5"},
		{"scheme-like first segment", "https://server.example", "evil:x/y", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &SEP2Client{baseURL: tc.base}
			got, err := c.putTarget(tc.href)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("putTarget(%q) = %q, want a refusal", tc.href, got)
				}
				if !strings.Contains(err.Error(), "not on the configured server") {
					t.Errorf("putTarget(%q) error = %v, want one naming the server", tc.href, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("putTarget(%q) = %q, %v; want %q", tc.href, got, err, tc.want)
			}
		})
	}
}
