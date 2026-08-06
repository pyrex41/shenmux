// Command secret-proxy runs the MITM secret-injection proxy.
//
//	secret-proxy --listen 127.0.0.1:PORT --secrets FILE --ca-dir DIR [--log FILE]
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/pyrex41/shenmux/orchestrator/secretproxy"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8888", "listen address host:port")
	secrets := flag.String("secrets", "", "path to secrets.json")
	caDir := flag.String("ca-dir", "", "directory holding ca.crt/ca.key (auto-generated if absent)")
	logPath := flag.String("log", "", "log file (default stderr)")
	flag.Parse()

	if *secrets == "" || *caDir == "" {
		log.Fatal("secret-proxy: --secrets and --ca-dir are required")
	}

	var logw io.Writer = os.Stderr
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("secret-proxy: cannot open log file: %v", err)
		}
		defer f.Close()
		logw = f
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := secretproxy.Run(ctx, *listen, *secrets, *caDir, logw); err != nil {
		log.Fatalf("secret-proxy: %v", err)
	}
}
