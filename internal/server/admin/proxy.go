package admin

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// newReverseProxy creates a reverse proxy that forwards requests to the
// ES API server (typically localhost:9200). The /api/proxy/ prefix is
// stripped before forwarding.
func newReverseProxy(esapiAddr string) (*httputil.ReverseProxy, error) {
	target := &url.URL{
		Scheme: "http",
		Host:   esapiAddr,
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		// Strip the /api/proxy prefix so the backend sees the
		// original ES API path (e.g. /_cluster/health).
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api/proxy")
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		// Remove the X-Forwarded-Prefix header set by the frontend
		// to avoid confusing the backend.
		req.Header.Del("X-Forwarded-Prefix")
	}

	return proxy, nil
}
