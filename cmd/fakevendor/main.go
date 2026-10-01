// Command fakevendor is a reverse proxy in front of the real Frankfurter
// API, with an admin endpoint to toggle it into a failure mode on
// demand. It exists purely for local chaos demos: fxservice's
// FRANKFURTER_BASE_URL is read once at startup, so there's no way to
// make the *real* vendor go down and come back up without restarting
// fxservice (which would also reset its cache, defeating the point of a
// stale-serving demo). Point fxservice at this proxy instead, and flip
// its mode with a curl call while fxservice keeps running.
//
// This will be reused for the later chaos-engineering phase too, with
// more failure modes as needed — kept deliberately simple for now.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

// mode is stored as an atomic int32 so the proxy handler (read on every
// request) and the admin handler (written rarely) don't need a mutex.
type mode int32

const (
	modeOff mode = iota
	modeError
	modeSlow
)

func (m mode) String() string {
	switch m {
	case modeError:
		return "error"
	case modeSlow:
		return "slow"
	default:
		return "off"
	}
}

func parseMode(s string) (mode, bool) {
	switch s {
	case "off", "":
		return modeOff, true
	case "error":
		return modeError, true
	case "slow":
		return modeSlow, true
	default:
		return modeOff, false
	}
}

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	upstream := flag.String("upstream", "https://api.frankfurter.dev/v1", "real vendor base URL to proxy to")
	slowDelay := flag.Duration("slow-delay", 6*time.Second, "response delay injected in slow mode (should exceed fxservice's UPSTREAM_TIMEOUT to actually trigger retries/breaker)")
	flag.Parse()

	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid -upstream URL: %v", err)
	}

	var current atomic.Int32 // holds a mode value

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Preserve the upstream's own Host header expectations.
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.Host = target.Host
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/admin/mode", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]string{"mode": mode(current.Load()).String()})
		case http.MethodPost:
			requested := r.URL.Query().Get("mode")
			m, ok := parseMode(requested)
			if !ok {
				http.Error(w, `{"error":"mode must be one of: off, error, slow"}`, http.StatusBadRequest)
				return
			}
			current.Store(int32(m))
			log.Printf("fakevendor: mode set to %q", m)
			writeJSON(w, map[string]string{"mode": m.String()})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch mode(current.Load()) {
		case modeError:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"fakevendor: injected failure (mode=error)"}`)
			return
		case modeSlow:
			time.Sleep(*slowDelay)
			// After the delay, still serve a real response — "slow" means
			// slow, not necessarily broken, which is a distinct failure
			// shape worth demoing separately from "error".
			proxy.ServeHTTP(w, r)
			return
		default:
			proxy.ServeHTTP(w, r)
		}
	})

	log.Printf("fakevendor: proxying to %s, listening on %s", *upstream, *addr)
	log.Printf("fakevendor: toggle with POST %s/admin/mode?mode=off|error|slow", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
