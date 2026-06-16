package servetls

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedirectHandler(t *testing.T) {
	tests := []struct {
		name     string
		tlsAddr  string
		host     string
		target   string // request-target (path?query)
		wantLoc  string
		wantCode int
	}{
		{
			name:     "default https port omitted",
			tlsAddr:  ":443",
			host:     "example.com",
			target:   "/config",
			wantLoc:  "https://example.com/config",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "non-standard port preserved",
			tlsAddr:  "0.0.0.0:8443",
			host:     "example.com",
			target:   "/reports?x=1",
			wantLoc:  "https://example.com:8443/reports?x=1",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "request host port stripped, tls port substituted",
			tlsAddr:  ":8443",
			host:     "example.com:80",
			target:   "/",
			wantLoc:  "https://example.com:8443/",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "no port in tls addr yields no port in target",
			tlsAddr:  "example.com",
			host:     "example.com",
			target:   "/x",
			wantLoc:  "https://example.com/x",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "path and query preserved",
			tlsAddr:  ":443",
			host:     "host.local",
			target:   "/a/b?c=d&e=f",
			wantLoc:  "https://host.local/a/b?c=d&e=f",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "ipv6 literal host with port rebracketed",
			tlsAddr:  ":8443",
			host:     "[::1]:80",
			target:   "/config",
			wantLoc:  "https://[::1]:8443/config",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "bare ipv6 literal host stays bracketed",
			tlsAddr:  ":443",
			host:     "[::1]",
			target:   "/config",
			wantLoc:  "https://[::1]/config",
			wantCode: http.StatusMovedPermanently,
		},
		{
			name:     "ephemeral port zero omitted from redirect",
			tlsAddr:  ":0",
			host:     "example.com:80",
			target:   "/setup",
			wantLoc:  "https://example.com/setup",
			wantCode: http.StatusMovedPermanently,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := RedirectHandler(tc.tlsAddr)
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+tc.target, nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d; want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("Location"); got != tc.wantLoc {
				t.Errorf("Location = %q; want %q", got, tc.wantLoc)
			}
		})
	}
}
