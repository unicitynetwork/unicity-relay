package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"
	"zooid/zooid"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// pprofAddrIsLoopback rejects PPROF_ADDR values that would expose pprof on a
// public interface. Documentation says "bind to localhost"; this enforces it
// at startup so a stray PPROF_ADDR=":6060" doesn't leak heap/goroutine dumps.
// Returns (ok, reason) — reason is empty when ok is true.
func pprofAddrIsLoopback(addr string) (bool, string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Sprintf("invalid host:port: %v", err)
	}
	if host == "" {
		// ":6060" with no host listens on all interfaces.
		return false, "host is empty (binds all interfaces); use 127.0.0.1 or [::1]"
	}
	if host == "localhost" {
		return true, ""
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false, fmt.Sprintf("could not resolve host %q: %v", host, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false, fmt.Sprintf("host %q resolves to non-loopback %s", host, ip)
		}
	}
	return true, ""
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// rootCtx is the single source of cancellation for the whole process.
	// Everything that runs inside zooid derives its context from this — no
	// other place in the codebase calls context.Background(). On SIGINT/
	// SIGTERM (or stop()) the ctx cancels, which propagates down the tree
	// to abort in-flight DB ops, fsnotify watcher, metric/retention loops,
	// etc. Avoiding scattered context.Background() calls also means
	// graceful shutdown actually stops in-flight goroutines instead of
	// letting them run their full per-call timeout (issue #18).
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		if ok, reason := pprofAddrIsLoopback(pprofAddr); !ok {
			log.Fatalf("refusing to start pprof on %q: %s — pprof must bind to a loopback address; use SSH/port-forward to access it remotely", pprofAddr, reason)
		}
		go func() {
			log.Printf("pprof server listening on %s\n", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("pprof server error: %v\n", err)
			}
		}()
	}

	go zooid.Start(rootCtx)
	zooid.StartMetricsCollector(rootCtx)
	zooid.StartRetentionCleaner(rootCtx)

	<-rootCtx.Done()

	log.Println("\nShutting down gracefully...")

	// Detached shutdown deadline — once SIGTERM has fired, rootCtx is
	// already canceled, so we need a fresh budget for the http server's
	// drain. This is the one legitimate context.Background() in the
	// process: we're past the lifetime of rootCtx and starting a new,
	// bounded shutdown phase.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v\n", err)
	}
}
