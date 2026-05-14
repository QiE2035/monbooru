//go:build !tagger

package main

import (
	"fmt"
	"os"
)

// runWorker errors on the non-tagger build: the subcommand only
// matters when inference is compiled in.
func runWorker(_ []string) {
	fmt.Fprintln(os.Stderr, "tagger-worker: this binary was built without -tags tagger")
	os.Exit(2)
}
