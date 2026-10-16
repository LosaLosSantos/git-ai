package main

import (
	"context"
	"fmt"
	"os"

	cli "github.com/feder-cr/git-ai/internal/cli/praefect"
)

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
