// Package profiling exposes Go's standard pprof endpoints on a dedicated
// listener, for Alloy's pyroscope.scrape to pull continuous profiles from.
//
// It is deliberately never wired onto the app's main mux. /debug/pprof/profile
// lets anyone who can reach it force the process to spend 30 seconds
// building a CPU profile, and /debug/pprof/heap or /goroutine hand back a
// dump of the process's own memory/stack state — harmless from inside the
// cluster, but a small DoS knob and an information leak if the same port is
// ever reachable from the internet. This app's main port is: some of these
// apps are public, and even the ones behind Cloudflare Access route every
// path on that port through the same tunnel. A second listener that is
// simply never added to a Service or a cloudflared route can't make that
// mistake regardless of what else changes on the main mux.
package profiling

import (
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
)

func init() {
	// Both default to 0 (disabled) in the Go runtime, which makes
	// /debug/pprof/mutex and /debug/pprof/block come back empty regardless
	// of how much contention or blocking is actually happening — a silent
	// gap rather than an error, easy to not notice until the profile you
	// need isn't there. The rates below are the commonly-recommended
	// values: sample every contended mutex, and one block-event sample per
	// ~10µs of blocking, which costs nothing measurable at this traffic
	// volume.
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(10000)
}

// Handler returns the standard net/http/pprof endpoints as their own mux,
// wired by hand rather than via net/http/pprof's package-level
// DefaultServeMux registration — none of these apps use
// http.DefaultServeMux, so that init-time side effect would register the
// handlers nowhere anyone serves.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	// The named profiles (heap, goroutine, allocs, block, mutex,
	// threadcreate) are exposed through pprof.Handler rather than the
	// functions above, which only cover the five special-cased paths.
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, pprof.Handler(name))
	}
	return mux
}

// ListenAndServe starts the debug server on addr and blocks. Errors are
// logged rather than fatal: profiling is diagnostic tooling, not a reason
// to take the whole app down if its own port can't bind.
func ListenAndServe(addr string) {
	if err := http.ListenAndServe(addr, Handler()); err != nil {
		log.Printf("profiling server on %s: %v", addr, err)
	}
}
