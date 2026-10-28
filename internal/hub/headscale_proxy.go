package hub

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Headscale behind the hub ([tailnet.headscale] proxy, on with
// sidecar_dir): Headscale listens on localhost only and the hub's own
// listeners forward exactly the endpoints tailscale nodes use, so Headscale
// shares the hub's domain and Let's Encrypt certificate and nothing else of
// it is reachable from outside (its REST API /api/v1, gRPC, metrics, the
// interactive /register and /oidc flows, swagger and the like stay local:
// the hub mints pre-auth keys over the API itself).
//
// The routes are Headscale v0.29's (hscontrol/app.go createRouter):
//
//	/ts2021                 TS2021 control protocol: an HTTP Upgrade
//	                        ("tailscale-control-protocol", POST; GET for the
//	                        WebSocket variant) to Noise; everything else
//	                        (register, map polls) runs inside it
//	/key                    the server's Noise public key (GET, before /ts2021)
//	/derp                   embedded DERP relay: Upgrade "DERP" or websocket
//	/derp/probe, /derp/latency-check   DERP reachability / HTTPS latency (netcheck)
//	/bootstrap-dns          DNS answers for nodes whose resolver fails
//	/machine/ping-response  HEAD callback of a control-initiated ping
//
// None of these collide with the hub's own routes (http.go).
var headscaleRoutes = []string{
	"POST /ts2021",
	"GET /ts2021",
	"GET /key",
	"/derp",
	"/derp/probe",
	"/derp/latency-check",
	"GET /bootstrap-dns",
	"HEAD /machine/ping-response",
}

// headscaleProxy forwards one request to Headscale at target. Upgrades
// (/ts2021, /derp) are handed through by httputil.ReverseProxy: Headscale
// answers 101 Switching Protocols, the proxy hijacks the client connection
// and copies bytes both ways (Hijack clears the server's deadlines; the
// hub's http.Servers only have a ReadHeaderTimeout).
func headscaleProxy(target *url.URL, log *slog.Logger) http.Handler {
	tr := &http.Transport{
		Proxy:                 nil, // localhost: never through an HTTP proxy from the environment
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// Headscale builds no URLs from Host, but keep what the node asked for
			pr.Out.Host = pr.In.Host
			// DERP "fast start" skips the 101 response when the client found
			// the DERP server's key in its TLS certificate. Here TLS is the
			// hub's (no such key: clients don't ask), and without a response
			// the proxy could not switch protocols: never forward it.
			pr.Out.Header.Del("Derp-Fast-Start")
		},
		Transport:     tr,
		FlushInterval: -1, // long polls and streams: no buffering
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("headscale proxy", "path", r.URL.Path, "err", err)
			http.Error(w, "headscale is not available", http.StatusBadGateway)
		},
	}
}

// withHeadscale serves Headscale's public routes in front of next (the hub's
// handler); everything else, including other methods on those paths, goes to
// next.
func withHeadscale(next http.Handler, proxy http.Handler) http.Handler {
	mux := http.NewServeMux()
	for _, p := range headscaleRoutes {
		mux.Handle(p, proxy)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			proxy.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withHeadscaleNoise adds the Noise upgrade to the plain-http redirect
// listener (:80). Tailscale clients try http://<host>/ts2021 first and
// https a moment later; the Noise handshake authenticates the server with
// the key the client fetched over https (/key), so plain http is safe here,
// and saves the clients the fallback delay.
func withHeadscaleNoise(next http.Handler, proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ts2021" && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
			proxy.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
