package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embed the tz database so time.LoadLocation works in the Alpine image,
	// which ships no zoneinfo.
	_ "time/tzdata"

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

	db, closeDB := rg.Must2(openDB(cfg.MySQL))
	defer func() {
		if err := closeDB(); err != nil {
			log.Println("mysql close:", err)
		}
	}()

	srv := &http.Server{
		Addr:              listen,
		Handler:           NewServer(cfg, newGormCallLogger(db, cfg.MySQL.detailRetain()), db).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Println("listening on", listen)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err = <-errCh:
		// ListenAndServe always returns a non-nil error; ErrServerClosed means
		// a clean Shutdown, which only fires after the signal branch below.
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return
	case <-ctx.Done():
		log.Println("shutting down")
	}

	// Unregister SIGINT/SIGTERM so a second signal uses the default terminate
	// action. Shutdown has no deadline: in-flight LLM streams must be allowed
	// to finish (or the client to disconnect).
	stop()
	err = srv.Shutdown(context.Background())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
