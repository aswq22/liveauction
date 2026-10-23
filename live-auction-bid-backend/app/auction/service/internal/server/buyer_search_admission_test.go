package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuyerSearchAdmissionBurstAndIndependentClients(t *testing.T) {
	called := 0
	handler := newSearchAdmissionFilter(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	request := func(remote, forward string) int {
		r := httptest.NewRequest("POST", "/api/buyer/lots/search", strings.NewReader(`{"query":"玉"}`))
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", forward)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 10; i++ {
		if got := request("192.0.2.1:1234", ""); got != 204 {
			t.Fatal(i, got)
		}
	}
	if request("192.0.2.1:1234", "198.51.100.4") != 429 {
		t.Fatal("spoofed forwarding bypassed burst limit")
	}
	if request("192.0.2.2:1234", "") != 204 {
		t.Fatal("independent client penalized")
	}
	if called != 11 {
		t.Fatal(called)
	}
}
func TestBuyerSearchAdmissionRejectsOversizedBeforeHandler(t *testing.T) {
	called := false
	h := newSearchAdmissionFilter(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	r := httptest.NewRequest("POST", "/api/buyer/lots/search", strings.NewReader(strings.Repeat("x", 17000)))
	r.RemoteAddr = "192.0.2.1:1234"
	r.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 || called {
		t.Fatal(w.Code, called)
	}
}
func TestBuyerSearchTrustedProxyUsesRightmostUntrustedHop(t *testing.T) {
	gate := newSearchAdmissionFilter([]string{"127.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/api/buyer/lots/search", strings.NewReader(`{}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", "203.0.113.1, 192.0.2.9")
		gate.ServeHTTP(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest("POST", "/api/buyer/lots/search", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.2, 192.0.2.9")
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatal("spoofed leftmost identity trusted", w.Code)
	}
}
