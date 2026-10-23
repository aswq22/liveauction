package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type searchBucket struct {
	tokens  float64
	updated time.Time
}

func (b *searchBucket) take(now time.Time, rate, burst float64) bool {
	if b.updated.IsZero() {
		b.tokens = burst
	} else {
		b.tokens += now.Sub(b.updated).Seconds() * rate
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.updated = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
func searchClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	remote, e := netip.ParseAddr(host)
	if e != nil {
		return "unknown"
	}
	isTrusted := func(ip netip.Addr) bool {
		for _, prefix := range trusted {
			if prefix.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	if !isTrusted(remote) {
		return remote.Unmap().String()
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	selected := remote
	for i := len(chain) - 1; i >= 0; i-- {
		ip, e := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if e != nil {
			return remote.Unmap().String()
		}
		selected = ip
		if !isTrusted(ip) {
			break
		}
	}
	return selected.Unmap().String()
}

// Trust only configured proxy CIDRs. Untrusted forwarding headers never create
// a new client identity. The map, body, burst and process-wide budget are bounded.
func newSearchAdmissionFilter(cidrs []string) func(http.Handler) http.Handler {
	trusted := []netip.Prefix{}
	for _, text := range cidrs {
		if strings.TrimSpace(text) == "" {
			continue
		}
		prefix, e := netip.ParsePrefix(strings.TrimSpace(text))
		if e != nil {
			panic("invalid AUCTION_SEARCH_TRUSTED_PROXIES CIDR")
		}
		trusted = append(trusted, prefix)
	}
	var mu sync.Mutex
	clients := map[string]*searchBucket{}
	global := searchBucket{}
	lastSweep := time.Time{}
	reject := func(w http.ResponseWriter, status, code int, message string) {
		w.Header().Set("Content-Type", "application/json")
		if status == 429 {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"code": code, "message": message}})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/buyer/lots/search" && r.URL.Path != "/api/ai/buyer/consult" {
				next.ServeHTTP(w, r)
				return
			}
			now := time.Now()
			ip := searchClientIP(r, trusted)
			mu.Lock()
			if now.Sub(lastSweep) > 30*time.Second {
				for key, b := range clients {
					if now.Sub(b.updated) > time.Minute {
						delete(clients, key)
					}
				}
				lastSweep = now
			}
			bucket := clients[ip]
			if bucket == nil && len(clients) < 10000 {
				bucket = &searchBucket{}
				clients[ip] = bucket
			}
			allowed := bucket != nil && bucket.take(now, 5, 10) && global.take(now, 100, 200)
			mu.Unlock()
			if !allowed {
				reject(w, 429, 429301, "SEARCH_RATE_LIMITED")
				return
			}
			const maxBody = 16 * 1024
			if r.ContentLength > maxBody {
				reject(w, 413, 400301, "SEARCH_BODY_TOO_LARGE")
				return
			}
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(now.Add(2 * time.Second))
			defer controller.SetReadDeadline(time.Time{})
			body, e := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			r.Body.Close()
			if len(body) > maxBody {
				reject(w, 413, 400301, "SEARCH_BODY_TOO_LARGE")
				return
			}
			if e != nil {
				reject(w, 400, 400301, "SEARCH_INVALID_BODY")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		})
	}
}
