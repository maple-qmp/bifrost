package handlers

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestLocalAccountRequestBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, remote, host, origin, site string
		allowed                          bool
	}{
		{"local", "127.0.0.1", "localhost:8080", "http://localhost:8080", "same-origin", true},
		{"local no browser", "127.0.0.1", "127.0.0.1:8080", "", "", true},
		{"remote", "192.0.2.1", "localhost:8080", "", "", false},
		{"dns rebinding", "127.0.0.1", "evil.test:8080", "", "", false},
		{"cross origin", "127.0.0.1", "localhost:8080", "https://evil.test", "cross-site", false},
		{"null origin", "127.0.0.1", "localhost:8080", "null", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req fasthttp.Request
			req.SetRequestURI("http://" + tc.host + "/api/providers/chatgpt/account")
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.site)
			var ctx fasthttp.RequestCtx
			ctx.Init(&req, &net.TCPAddr{IP: net.ParseIP(tc.remote), Port: 10000}, nil)
			require.Equal(t, tc.allowed, localAccountRequest(&ctx))
		})
	}
}
