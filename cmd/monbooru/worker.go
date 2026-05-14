//go:build tagger

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tagger"
)

// runWorker is the body of the `monbooru tagger-worker` subcommand.
// Spawned by the parent's IPC backend; serves that parent's requests
// until the socket closes or SIGTERM arrives.
func runWorker(argv []string) {
	fs := flag.NewFlagSet("tagger-worker", flag.ExitOnError)
	socket := fs.String("socket", "", "parent Unix-domain socket to connect to")
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintf(os.Stderr, "tagger-worker: %v\n", err)
		os.Exit(2)
	}
	if *socket == "" {
		fmt.Fprintf(os.Stderr, "tagger-worker: --socket is required\n")
		os.Exit(2)
	}
	// Force in-process regardless of the parent's MONBOORU_TAGGER_BACKEND;
	// the worker is the in-process executor by definition and spawning a
	// grandchild would loop.
	tagger.UseInprocBackend()
	logx.Set(os.Getenv("MONBOORU_TAGGER_WORKER_LOG"))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := tagger.RunWorkerServer(ctx, *socket); err != nil {
		fmt.Fprintf(os.Stderr, "tagger-worker: %v\n", err)
		os.Exit(1)
	}
}
