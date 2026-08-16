package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// newGrafanaProxy proxies /grafana/* through to a Grafana running on the
// same host, so embedded panels are same-origin with the app.
//
// Same-origin matters for more than tidiness. An iframe pointing straight
// at a second port would have to name a host, and this app is reached by
// several: jcwpi on the LAN, a Tailscale address remotely, localhost in
// development. A relative /grafana/... URL is correct from all of them. It
// also sidesteps X-Frame-Options, CSP frame-ancestors, and cookie SameSite
// entirely, and lets Grafana bind to 127.0.0.1 so it is reachable only
// through this app rather than being a second open port on the network.
//
// The path is passed through unchanged rather than stripped: Grafana is
// configured with serve_from_sub_path, so it expects to see the /grafana
// prefix and generates its own asset URLs to match.
func newGrafanaProxy(target string) (http.Handler, error) {
	base, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse grafana url %q: %w", target, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("grafana url %q needs a scheme and host, e.g. http://127.0.0.1:3000", target)
	}

	proxy := httputil.NewSingleHostReverseProxy(base)

	// Without this, a stopped Grafana surfaces as a bare 502 in an iframe,
	// which reads as "the app is broken" rather than "the graph backend is
	// down". The response is deliberately plain HTML: it is being rendered
	// inside the frame where the chart should be.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("grafana proxy: %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `<!doctype html><html><body style="font:14px system-ui;padding:1rem;color:#888">`+
			`<p>Grafana is not responding at %s.</p>`+
			`<p>Everything else in the app still works — only the graph needs it.</p>`+
			`</body></html>`, base)
	}

	return proxy, nil
}
