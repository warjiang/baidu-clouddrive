package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/warjiang/baidu-clouddrive/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root := cli.New(version)
	executed, err := root.ExecuteContextC(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		code := cli.ExitCode(err)
		if executed == root && code == 1 {
			code = 252 // Unknown command or root-level usage error.
		}
		os.Exit(code)
	}
}
