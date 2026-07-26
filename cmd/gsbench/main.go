package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"gstop/internal/gsbench"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(gsbench.RunCLI(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
