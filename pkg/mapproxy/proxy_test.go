package mapproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHandler_ProxiesSamePathAndQuery(t *testing.T) {
	// Upstream mock: echoes path+query and returns a PNG-like payload
	var gotPath, gotRawQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	t.Cleanup(upstream.Close)

	// Convert to base (scheme+host)
	u, _ := url.Parse(upstream.URL)
	base := u.Scheme + "://" + u.Host

	h, err := Handler(base)
	if err != nil {
		t.Fatalf("Handler() error: %v", err)
	}

	// Client -> proxy
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)

	resp, err := http.Get(proxy.URL + "/map/0/0/0.png?t=1756728782772")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/map/0/0/0.png" {
		t.Fatalf("upstream path mismatch: got %q", gotPath)
	}
	if gotRawQuery != "t=1756728782772" {
		t.Fatalf("upstream rawquery mismatch: got %q", gotRawQuery)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type mismatch: got %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if len(b) < 4 || b[0] != 0x89 || b[1] != 'P' || b[2] != 'N' || b[3] != 'G' {
		t.Fatalf("body not proxied correctly: %v", b)
	}
}
