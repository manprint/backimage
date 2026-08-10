package main

import (
	"context"
	"os"

	"github.com/manprint/backimage/internal/cli"
)

func main() {
	ctx := context.Background()
	if err := cli.Execute(ctx, os.Args[1:]); err != nil {
		cli.ReportError(err)
		os.Exit(cli.ExitCodeFor(err))
	}
}
