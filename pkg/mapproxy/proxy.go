package mapproxy

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Handler は `/map/` 以下のパスを、同一パス・同一クエリのまま
// 指定した上流サーバーへプロキシ転送します。
// 例: upstream = "http://10.0.0.1:8080" のとき、
//
//	/map/0/0/0.png?t=123 -> http://10.0.0.1:8080/map/0/0/0.png?t=123
func Handler(upstream string, opts ...Option) (http.Handler, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errors.New("mapproxy: upstream must include scheme and host, e.g. http://host:8080")
	}
	cfg := config{
		dialTimeout:           5 * time.Second,
		tlsTimeout:            5 * time.Second,
		idleConn:              100,
		idleConnPerHost:       100,
		respHeaderTimeout:     10 * time.Second,
		expectContinueTimeout: 1 * time.Second,
		requestTimeout:        15 * time.Second,
		allowPrefixes:         []string{"/map/"},
	}
	for _, f := range opts {
		f(&cfg)
	}

	// Transport with sensible timeouts.
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.idleConn,
		MaxIdleConnsPerHost:   cfg.idleConnPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.tlsTimeout,
		ResponseHeaderTimeout: cfg.respHeaderTimeout,
		ExpectContinueTimeout: cfg.expectContinueTimeout,
	}

	director := func(req *http.Request) {
		// 元のパスとクエリを温存しつつ、上流スキーム/ホストに付け替える
		req.URL.Scheme = u.Scheme
		req.URL.Host = u.Host
		// パスはそのまま（/map/...）を転送
		// RawPath もあれば維持
		if req.URL.RawPath == "" {
			req.URL.RawPath = req.URL.EscapedPath()
		}
		// Host ヘッダも上流へ合わせる（多くのサーバーで必須）
		req.Host = u.Host
		// X-Forwarded-*
		if ip, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+ip)
			} else {
				req.Header.Set("X-Forwarded-For", ip)
			}
		}
		req.Header.Set("X-Forwarded-Proto", req.URL.Scheme)
	}

	rp := &httputil.ReverseProxy{
		Director:  director,
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			// ログだけ出して簡潔に 502
			log.Printf("mapproxy: upstream error for %s: %v", r.URL.String(), e)
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			// 画像はそのまま通す。追加のヘッダ調整が必要ならここで行う。
			return nil
		},
	}

	// ルーティング制御: 指定プレフィックスのみ許可
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasAnyPrefix(r.URL.Path, cfg.allowPrefixes) {
			http.NotFound(w, r)
			return
		}
		// 上流への全体タイムアウト
		ctx, cancel := context.WithTimeout(r.Context(), cfg.requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		rp.ServeHTTP(w, r)
	}), nil
}

func hasAnyPrefix(p string, prefixes []string) bool {
	for _, pref := range prefixes {
		if strings.HasPrefix(p, pref) {
			return true
		}
	}
	return false
}

// オプション
type config struct {
	dialTimeout           time.Duration
	tlsTimeout            time.Duration
	idleConn              int
	idleConnPerHost       int
	respHeaderTimeout     time.Duration
	expectContinueTimeout time.Duration
	requestTimeout        time.Duration
	allowPrefixes         []string
}

type Option func(*config)

func WithRequestTimeout(d time.Duration) Option { return func(c *config) { c.requestTimeout = d } }
func WithAllowedPrefixes(prefixes ...string) Option {
	return func(c *config) { c.allowPrefixes = append([]string{}, prefixes...) }
}
func WithDialTimeout(d time.Duration) Option         { return func(c *config) { c.dialTimeout = d } }
func WithTLSHandshakeTimeout(d time.Duration) Option { return func(c *config) { c.tlsTimeout = d } }
func WithResponseHeaderTimeout(d time.Duration) Option {
	return func(c *config) { c.respHeaderTimeout = d }
}
func WithExpectContinueTimeout(d time.Duration) Option {
	return func(c *config) { c.expectContinueTimeout = d }
}
func WithMaxIdleConns(total, perHost int) Option {
	return func(c *config) { c.idleConn, c.idleConnPerHost = total, perHost }
}
