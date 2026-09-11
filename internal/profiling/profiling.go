// Package profiling exposes Go's pprof endpoints on a dedicated listener for
// Alloy's pyroscope.scrape.
//
// Never wire these onto the app's main mux. /debug/pprof/profile lets any
// caller burn 30s of CPU, and heap/goroutine dump process state — fine inside
// the cluster, not fine on a port a tunnel serves to the internet. A listener
// that is never added to a Service can't leak by accident.
package profiling

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
)

func init() {
	// Both default to 0, which makes /debug/pprof/mutex and /block come back
	// empty rather than erroring. These are the recommended rates.
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(10000)
}

// Handler wires the pprof endpoints by hand; net/http/pprof's init registers
// them on http.DefaultServeMux, which nothing here serves.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	// pprof.Index only special-cases the five paths above; the named profiles
	// go through pprof.Handler.
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, pprof.Handler(name))
	}
	return mux
}

// ListenAndServe blocks. A bind failure is logged, not fatal — diagnostics
// are not worth taking the app down for.
func ListenAndServe(addr string) {
	if err := http.ListenAndServe(addr, Handler()); err != nil {
		slog.Error("profiling server", "addr", addr, "error", err)
	}
}
