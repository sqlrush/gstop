package main

import (
	"context"
	"os"

	"gstop/internal/gsbench"
)

// commandContext intentionally leaves SIGINT and SIGTERM to the operating
// system. A single Ctrl+C must terminate gsbench immediately instead of
// canceling into the automatic restore path.
func commandContext() context.Context {
	return context.Background()
}

func main() {
	os.Exit(gsbench.RunCLI(
		commandContext(),
		os.Args[1:],
		os.Stdout,
		os.Stderr,
	))
}
