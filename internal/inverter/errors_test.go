package inverter

import (
	"errors"
	"net/http"
	"testing"
)

// classifyResponse unit tests (IEEE-046).
//
// In-package _test.go so unexported classifyResponse is reachable without an
// _export_test.go shim. Exercises the pure status-code → typed-error mapping
// independent of the TLS / network surface — the integration test cases live
// in the matching client_router_test.go (package inverter_test) which drives
// real Get / Post / Put through a gotls listener.

func TestClassifyResponse_Success(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code int
	}{
		{"200 OK", http.StatusOK},
		{"201 Created", http.StatusCreated},
		{"204 No Content", http.StatusNoContent},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: tc.code, Header: http.Header{}}
			if err := classifyResponse(resp); err != nil {
				t.Fatalf("classifyResponse(%d) = %v, want nil", tc.code, err)
			}
		})
	}
}

func TestClassifyResponse_TypedSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		code   int
		target error
	}{
		{"400 → ErrBadRequest", http.StatusBadRequest, ErrBadRequest},
		{"404 → ErrNotFound", http.StatusNotFound, ErrNotFound},
		{"405 → ErrMethodNotAllowed", http.StatusMethodNotAllowed, ErrMethodNotAllowed},
		{"501 → ErrNotImplemented", http.StatusNotImplemented, ErrNotImplemented},
		{"500 → ErrResponseTransient", http.StatusInternalServerError, ErrResponseTransient},
		{"503 → ErrResponseTransient", http.StatusServiceUnavailable, ErrResponseTransient},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: tc.code, Header: http.Header{}}
			err := classifyResponse(resp)
			if err == nil {
				t.Fatalf("classifyResponse(%d) = nil, want %v", tc.code, tc.target)
			}
			if !errors.Is(err, tc.target) {
				t.Fatalf("classifyResponse(%d) = %v, want errors.Is %v", tc.code, err, tc.target)
			}
		})
	}
}

func TestClassifyResponse_Moved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code int
	}{
		{"301 Moved Permanently", http.StatusMovedPermanently},
		{"302 Found", http.StatusFound},
		{"307 Temporary Redirect", http.StatusTemporaryRedirect},
		{"308 Permanent Redirect", http.StatusPermanentRedirect},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hdr := http.Header{}
			hdr.Set("Location", "/v2/edev")
			resp := &http.Response{StatusCode: tc.code, Header: hdr}
			err := classifyResponse(resp)
			if err == nil {
				t.Fatalf("classifyResponse(%d) = nil, want *MovedError", tc.code)
			}
			var me *MovedError
			if !errors.As(err, &me) {
				t.Fatalf("classifyResponse(%d) = %v, want errors.As *MovedError", tc.code, err)
			}
			if me.Status != tc.code {
				t.Errorf("MovedError.Status = %d, want %d", me.Status, tc.code)
			}
			if me.Location != "/v2/edev" {
				t.Errorf("MovedError.Location = %q, want /v2/edev", me.Location)
			}
		})
	}
}

func TestClassifyResponse_MovedWithoutLocation(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{}}
	err := classifyResponse(resp)
	var me *MovedError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want *MovedError", err)
	}
	if me.Location != "" {
		t.Errorf("Location = %q, want empty", me.Location)
	}
	// Error message should still be safe to print.
	if msg := err.Error(); msg == "" {
		t.Error("Error() returned empty string for headerless 301")
	}
}

func TestClassifyResponse_UnmappedClientError(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}}
	err := classifyResponse(resp)
	if err == nil {
		t.Fatal("classifyResponse(401) = nil, want error")
	}
	// 401 is not currently mapped to a sentinel; it must NOT collide with
	// the typed sentinels, and it must NOT be transient.
	if errors.Is(err, ErrBadRequest) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrMethodNotAllowed) || errors.Is(err, ErrNotImplemented) ||
		errors.Is(err, ErrResponseTransient) {
		t.Errorf("401 unexpectedly matched a typed sentinel: %v", err)
	}
}

func TestClassifyResponse_UnmappedServerError(t *testing.T) {
	t.Parallel()
	// 599 is a non-standard server error band entry — must still map to
	// ErrResponseTransient so callers do not have to enumerate each code.
	resp := &http.Response{StatusCode: 599, Header: http.Header{}}
	err := classifyResponse(resp)
	if !errors.Is(err, ErrResponseTransient) {
		t.Errorf("599 → %v, want errors.Is ErrResponseTransient", err)
	}
}

func TestMovedError_ErrorMessage(t *testing.T) {
	t.Parallel()
	me := &MovedError{Location: "/v2/edev", Status: 301}
	want := "moved (301) to /v2/edev"
	if got := me.Error(); got != want {
		t.Errorf("MovedError.Error() = %q, want %q", got, want)
	}
}
