package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"
	"zooid/zooid"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	port := zooid.Env("PORT")
	metricsHandler := promhttp.Handler()
	srv := &http.Server{
		Addr: fmt.Sprintf(":%s", port),
		Handler: http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metrics" {
					metricsHandler.ServeHTTP(w, r)
					return
				}

				instance, exists := zooid.Dispatch(r.Host)
				if exists {
					instance.Relay.ServeHTTP(w, r)
				} else {
					http.Error(w, "Not Found", http.StatusNotFound)
				}
			},
		),
	}

	go func() {
		log.Printf("running on :%s\n", port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v\n", err)
		}
	}()

	// Optional pprof server on a separate port. Bind it to localhost only and
	// expose via SSH/port-forward in production — the runtime/debug endpoints
	// must not be reachable from the public internet (issue #18 needed
	// `goroutine?debug=2` from a leaking task to localize the leak).
	if pprofAddr := os.Getenv("PPROF_ADDR"); pprofAddr != "" {
		go func() {
			log.Printf("pprof server listening on %s\n", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("pprof server error: %v\n", err)
			}
		}()
	}

	go zooid.Start()
	zooid.StartMetricsCollector()
	zooid.StartRetentionCleaner()

	<-shutdown

	log.Println("\nShutting down gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v\n", err)
	}
}
