package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pyrex41/shenmux/naming"
	"github.com/pyrex41/shenmux/internal/server"
	"github.com/pyrex41/shenmux/internal/shenguard"
)

func main() {
	log.SetFlags(0)
	var (
		session   = flag.String("session", "default", "session name")
		control   = flag.String("control", "", "ZeroMQ control endpoint (ROUTER)")
		data      = flag.String("data", "", "ZeroMQ data endpoint (XPUB)")
		cols      = flag.Int("cols", 80, "initial columns")
		rows      = flag.Int("rows", 24, "initial rows")
		shell     = flag.String("shell", "auto", "default shell when no command is supplied (auto, fish, zsh, bash, or a path)")
		keepalive = flag.Bool("keepalive", false, "restart the default login shell after it exits")
		grace     = flag.Duration("exit-grace", 150*time.Millisecond, "time to leave sockets open after command exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: muxd [flags] [-- command [args...]]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := naming.ValidateSession(*session); err != nil {
		log.Fatal(err)
	}
	defaultControl, defaultData, err := naming.DefaultEndpoints(*session)
	if err != nil {
		log.Fatal(err)
	}
	if *control == "" {
		*control = defaultControl
	}
	if *data == "" {
		*data = defaultData
	}
	dim, err := shenguard.NewDimensions(*cols, *rows)
	if err != nil {
		log.Fatal(err)
	}
	command := flag.Args()
	defaultShell := len(command) == 0
	env := os.Environ()
	if defaultShell {
		command, err = defaultShellCommand(*shell)
		if err != nil {
			log.Fatal(err)
		}
	}
	env, cleanupShell, err := prepareShellEnvironment(command, env)
	if err != nil {
		log.Fatal(err)
	}
	if *keepalive && defaultShell {
		command, err = keepaliveShellCommand(command)
		if err != nil {
			log.Fatal(err)
		}
	}
	defer cleanupShell()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("shenmux session=%s control=%s data=%s command=%q", *session, *control, *data, command)
	if err := server.Serve(ctx, server.Config{
		Session: *session, ControlEndpoint: *control, DataEndpoint: *data,
		Dimensions: dim, Command: command, Env: env, ExitGrace: *grace,
	}); err != nil {
		log.Fatal(err)
	}
}
