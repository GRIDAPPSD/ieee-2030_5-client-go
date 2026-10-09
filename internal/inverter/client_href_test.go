package inverter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
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
				if !errors.Is(err, ErrHrefOffServer) || !strings.Contains(err.Error(), "not on the configured server") {
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

// A relative href keeps the configured base path, as Get and Post do.
func TestPutTarget_KeepsTheBasePath(t *testing.T) {
	c := &SEP2Client{baseURL: "https://h:8443/sep2"}
	for href, want := range map[string]string{
		"/edev/1/der/1/dercap": "https://h:8443/sep2/edev/1/der/1/dercap",
		"edev/1/frq/5":         "https://h:8443/sep2/edev/1/frq/5",
		"https://h:8443/x":     "https://h:8443/x",
	} {
		got, err := c.putTarget(href)
		if err != nil || got != want {
			t.Errorf("putTarget(%q) = %q, %v; want %q", href, got, err, want)
		}
	}
	for _, href := range []string{"//evil.example/x", "https://evil.example/x"} {
		if got, err := c.putTarget(href); !errors.Is(err, ErrHrefOffServer) {
			t.Errorf("putTarget(%q) = %q, %v; want a refusal", href, got, err)
		}
	}
}

// The requests a base URL with a path sends: a der-role DER PUT and an
// aggregator's withdrawal both land under the base path, as a GET does.
func TestPut_KeepsTheBasePathOnTheWire(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := newTestSEP2Client(t, srv.URL+"/sep2", srv.Client().Transport)
	if err := c.PutDERCapability(context.Background(), "/edev/1/der/1/dercap", sep2.DERCapability{}); err != nil {
		t.Fatalf("PutDERCapability: %v", err)
	}
	c.guard = guard.New(guard.RoleAggregator, c.lfdi, guard.NewStaticManagedSet())
	if err := c.PutFlowReservationRequest(context.Background(), "/edev/1/frq/5", sep2.FlowReservationRequest{}); err != nil {
		t.Fatalf("PutFlowReservationRequest: %v", err)
	}
	want := []string{"PUT /sep2/edev/1/der/1/dercap", "PUT /sep2/edev/1/frq/5"}
	if !slices.Equal(paths, want) {
		t.Errorf("server saw %q, want %q", paths, want)
	}
}
