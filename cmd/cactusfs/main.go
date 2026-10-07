// Command cactusfs is the single CactusFS binary. It is the composition root:
// the only place where adapters are constructed and wired to use cases.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `CactusFS — S3-compatible, erasure-coded object store.

Usage:
  cactusfs <command> [flags]

Commands:
  server     run a CactusFS node
  version    print version information
  help       show this help

Run "cactusfs server --help" for the flags and CACTUSFS_* variables of a command.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run executes one command and returns the process exit code. Everything the
// process reads from or writes to its environment is passed in, so run can be
// tested without touching globals.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return exitUsage
	}

	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help", "-h", "-help", "--help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case "server":
		return runServerCommand(ctx, args[1:], stdout, stderr, getenv)
	default:
		_, _ = fmt.Fprintf(stderr, "cactusfs: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}

func runServerCommand(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	cfg, err := parseServerConfig(args, getenv, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errFlagParse):
		// The flag package has already reported the problem on stderr.
		return exitUsage
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "cactusfs server: %v\n", err)
		return exitUsage
	}

	if err := runServer(ctx, cfg, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "cactusfs server: %v\n", err)
		return exitError
	}
	return exitOK
}
