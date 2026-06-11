package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// runHealthcheck is the body of the `monbooru healthcheck` subcommand. It
// GETs the local /health endpoint and exits 0 on a 2xx, non-zero otherwise.
func runHealthcheck(argv []string) {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	configPath := fs.String("config", "", "optional monbooru.toml to read server.bind_address from")
	timeout := fs.Duration("timeout", 3*time.Second, "probe timeout")
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(2)
	}

	url := "http://" + resolveHealthAddr(*configPath) + "/health"
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s -> %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
}

// resolveHealthAddr picks the address to probe the same way the server picks
// its bind address, but WITHOUT config.Load's side effect of rewriting the
// TOML on every call (a healthcheck runs every interval).
func resolveHealthAddr(configPath string) string {
	addr := os.Getenv("MONBOORU_SERVER_BIND_ADDRESS")
	if addr == "" && configPath != "" {
		var mc struct {
			Server struct {
				BindAddress string `toml:"bind_address"`
			} `toml:"server"`
		}
		if _, err := toml.DecodeFile(configPath, &mc); err == nil {
			addr = mc.Server.BindAddress
		}
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // best-effort: let the dialer reject it
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
