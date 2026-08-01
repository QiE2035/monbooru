//go:build tagger

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// runWorker is the body of the `monbooru tagger-worker` subcommand.
// Spawned by the parent's IPC backend; serves that parent's requests
// until the parent sends a graceful shutdown request or the connection
// closes. SIGINT/Ctrl+C is still honoured for local operator control.
func runWorker(argv []string) {
	fs := flag.NewFlagSet("tagger-worker", flag.ExitOnError)
	addr := fs.String("addr", "", "parent TCP address to connect to")
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintf(os.Stderr, "tagger-worker: %v\n", err)
		os.Exit(2)
	}
	if *addr == "" {
		fmt.Fprintf(os.Stderr, "tagger-worker: --addr is required\n")
		os.Exit(2)
	}
	// Force in-process regardless of the parent's MONBOORU_TAGGER_BACKEND;
	// the worker is the in-process executor by definition and spawning a
	// grandchild would loop.
	tagger.UseInprocBackend()
	logx.Set(os.Getenv("MONBOORU_TAGGER_WORKER_LOG"))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer cancel()

	if err := tagger.RunWorkerServer(ctx, *addr); err != nil {
		fmt.Fprintf(os.Stderr, "tagger-worker: %v\n", err)
		os.Exit(1)
	}
}
