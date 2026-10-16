package main

import (
	"context"
	"os"

	cli "github.com/feder-cr/git-ai/internal/cli/gitalybackup"
)

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		os.Exit(1)
	}
}
