// Command ports manages host-port allocations for Docker Compose stacks.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/cmerk2021/ports/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
