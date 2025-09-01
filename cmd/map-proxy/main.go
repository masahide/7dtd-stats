package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/masahide/7dtd-stats/pkg/mapproxy"
)

func main() {
	var (
		listen   string
		upstream string
	)
	flag.StringVar(&listen, "listen", ":8081", "listen address (e.g. :8081)")
	flag.StringVar(&upstream, "upstream", os.Getenv("UPSTREAM_BASE_URL"), "upstream base URL (e.g. http://host:8080)")
	flag.Parse()

	if upstream == "" {
		fmt.Fprintln(os.Stderr, "-upstream or UPSTREAM_BASE_URL is required")
		os.Exit(2)
	}

	h, err := mapproxy.Handler(upstream,
		mapproxy.WithRequestTimeout(15*time.Second),
		mapproxy.WithAllowedPrefixes("/map/"),
	)
	if err != nil {
		log.Fatalf("failed to init proxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/map/", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: listen, Handler: mux}
	log.Printf("map-proxy listening on %s -> %s (paths: /map/)", listen, upstream)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
