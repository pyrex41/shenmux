package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/pyrex41/shenmux/internal/naming"
	"github.com/pyrex41/shenmux/internal/webgateway"
)

func main() {
	session := flag.String("session", "default", "shenmux session")
	listen := flag.String("listen", ":8787", "HTTP listen address")
	control := flag.String("control", "", "muxd control endpoint")
	data := flag.String("data", "", "muxd data endpoint")
	flag.Parse()
	controlDefault, dataDefault, err := naming.DefaultEndpoints(*session)
	if err != nil {
		log.Fatal(err)
	}
	if *control == "" {
		*control = controlDefault
	}
	if *data == "" {
		*data = dataDefault
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: webgateway.New(ctx, webgateway.Config{
		Session: *session, ControlEndpoint: *control, DataEndpoint: *data,
	}).Handler()}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	log.Printf("shenmux web session=%s listen=http://%s", *session, *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
