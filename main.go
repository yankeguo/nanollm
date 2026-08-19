package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yankeguo/rg"
)

func main() {
	var err error
	defer func() {
		if err == nil {
			return
		}
		log.Println("exited with error:", err)
		os.Exit(1)
	}()
	defer rg.Guard(&err)

	configPath := envOr("NANOLLM_CONFIG", "config.yaml")
	listen := envOr("NANOLLM_LISTEN", ":8080")
	flag.StringVar(&configPath, "config", configPath, "path to yaml config")
	flag.StringVar(&listen, "listen", listen, "http listen address")
	flag.Parse()

	cfg := rg.Must(loadConfig(configPath))
	log.Printf("loaded %d models from %s", len(cfg.Models), configPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics, shutdownMetrics := rg.Must2(setupMetrics(ctx))
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownMetrics(sctx); err != nil {
			log.Println("metrics shutdown:", err)
		}
	}()

	srv := &http.Server{
		Addr:              listen,
		Handler:           NewServer(cfg, metrics).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Println("listening on", listen)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err = <-errCh:
		return
	case <-ctx.Done():
		log.Println("shutting down")
	}

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
