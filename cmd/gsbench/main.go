package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"gstop/internal/gsbench"
)

func commandContextFromSignal(
	signals <-chan os.Signal,
	restoreDefaults func(),
) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-signals:
			if restoreDefaults != nil {
				restoreDefaults()
			}
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

// commandContext turns the first Ctrl+C into graceful cancellation so Runner
// can stop tagged workloads and restore state. Signal defaults are restored
// immediately, so a second interrupt still terminates a stuck process.
func commandContext() context.Context {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	return commandContextFromSignal(signals, func() {
		signal.Stop(signals)
	})
}

func main() {
	os.Exit(gsbench.RunCLI(
		commandContext(),
		os.Args[1:],
		os.Stdout,
		os.Stderr,
	))
}
