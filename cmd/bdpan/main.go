package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/warjiang/baidu-clouddrive/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := cli.New(version).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
