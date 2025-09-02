package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/masahide/7dtd-stats/pkg/mapproxy"
	"github.com/masahide/7dtd-stats/pkg/sse"
)

// Config はサービス起動に必要な設定です。
type Config struct {
	Listen          string        // 例: ":8081"
	UpstreamBaseURL string        // 例: "http://game:8080"
	StaticDir       string        // 例: "./web"（空なら無効）
	ShutdownTimeout time.Duration // 例: 5s
}

func loadConfig() Config {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", getEnv("LISTEN_ADDR", ":8081"), "listen address (e.g. :8081)")
	flag.StringVar(&cfg.UpstreamBaseURL, "upstream", getEnv("UPSTREAM_BASE_URL", ""), "upstream base URL (e.g. http://host:8080)")
	flag.StringVar(&cfg.StaticDir, "static-dir", getEnv("STATIC_DIR", ""), "path to static contents (optional)")
	var shutdownSec int
	flag.IntVar(&shutdownSec, "shutdown-timeout", getEnvInt("SHUTDOWN_TIMEOUT_SEC", 5), "graceful shutdown timeout seconds")
	flag.Parse()
	cfg.ShutdownTimeout = time.Duration(shutdownSec) * time.Second
	return cfg
}

func main() {
	cfg := loadConfig()
	if cfg.UpstreamBaseURL == "" {
		fmt.Fprintln(os.Stderr, "-upstream or UPSTREAM_BASE_URL is required")
		os.Exit(2)
	}

	// SSE Hub（replay/ping 対応）。現時点では外部入力が無いので ping のみ送出。
	hub := sse.NewHub(
		sse.WithReplay(256),
		sse.WithPingInterval(15*time.Second),
		sse.WithClientBuffer(64),
	)
	go hub.Run()
	defer hub.Close()

	// "Tile Proxy/Cache" 相当（/map/* のみ許可）。他機能は未実装だが、土台のルータ構成を先に用意。
	mapHandler, err := mapproxy.Handler(cfg.UpstreamBaseURL,
		mapproxy.WithRequestTimeout(15*time.Second),
		mapproxy.WithAllowedPrefixes("/map/"),
	)
	if err != nil {
		log.Fatalf("failed to init map proxy: %v", err)
	}

	mux := http.NewServeMux()

	// Map tiles (/map/{z}/{x}/{y}.png)
	mux.Handle("/map/", mapHandler)

	// Health/Ready endpoints
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// SSE: /sse/live
	mux.Handle("/sse/live", http.HandlerFunc(hub.ServeHTTP))
	// Future endpoints (未実装の土台): REST
	mux.HandleFunc("/api/map/info", notImplemented)
	mux.HandleFunc("/api/history/tracks", notImplemented)
	mux.HandleFunc("/api/history/events", notImplemented)

	// Root/Static (オプショナル)。指定時のみ有効化。
	if d := cfg.StaticDir; d != "" {
		// セキュリティ: ディレクトリが存在するときのみ公開
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			fs := http.FileServer(http.Dir(d))
			// SvelteKit の一般的な構成を想定し、"/" 直下で配信
			mux.Handle("/", fs)
		} else {
			abs, _ := filepath.Abs(d)
			log.Printf("warn: static-dir not found or not a dir: %s", abs)
		}
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "7dtd-stats server\n\n")
			fmt.Fprintf(w, "- /map/{z}/{x}/{y}.png  -> proxied to upstream\n")
			fmt.Fprintf(w, "- /healthz, /readyz\n")
			fmt.Fprintf(w, "- /sse/live (501), /api/map/info (501)\n")
			fmt.Fprintf(w, "- /api/history/tracks (501), /api/history/events (501)\n")
		})
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 起動ログ
	log.Printf("starting server on %s -> %s (paths: /map/)", cfg.Listen, cfg.UpstreamBaseURL)

	// Graceful shutdown
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
		_ = srv.Close()
	}
	log.Printf("shutdown complete")
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var i int
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
			return i
		}
	}
	return def
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
}
