package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	internalweb "github.com/leqwin/monbooru/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Subcommand dispatch happens before flag.Parse so the
	// subcommand's own flag set gets the argv tail unchanged.
	if len(os.Args) >= 2 && os.Args[1] == "tagger-worker" {
		runWorker(os.Args[2:])
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "healthcheck" {
		runHealthcheck(os.Args[2:])
		return
	}

	configPath := flag.String("config", "./monbooru.toml", "path to monbooru.toml config file")
	hashPassword := flag.String("hash-password", "", "print bcrypt hash of the given password and exit")
	flag.Parse()

	if *hashPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*hashPassword), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error hashing password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("FATAL loading config: %v", err)
	}
	logx.Set(cfg.Log.Level)
	logx.Infof("config: bind=%s galleries=%d default=%q models=%s log=%s",
		cfg.Server.BindAddress, len(cfg.Galleries), cfg.DefaultGallery, cfg.Paths.ModelPath, cfg.Log.Level)

	jobManager := jobs.NewManager()

	srv, err := internalweb.NewServer(cfg, *configPath, jobManager)
	if err != nil {
		log.Fatalf("FATAL creating web server: %v", err)
	}
	defer srv.Close()

	srv.StartWatchers()

	httpSrv := &http.Server{
		Addr:        cfg.Server.BindAddress,
		Handler:     srv.Handler(),
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is intentionally unset: bulk operations like delete-all
		// or re-extract can run for many minutes on large libraries. Slow
		// handlers are bounded by DB and filesystem latency.
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	srvErr := make(chan error, 1)
	go func() {
		logx.Infof("monbooru listening on %s", cfg.Server.BindAddress)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	// Report a bind failure through the channel rather than log.Fatalf so the
	// deferred srv.Close() still flushes the DB pools and stops the watchers.
	select {
	case <-quit:
		logx.Infof("shutting down...")
	case err := <-srvErr:
		logx.Errorf("FATAL HTTP server: %v", err)
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx) //nolint:errcheck
}
