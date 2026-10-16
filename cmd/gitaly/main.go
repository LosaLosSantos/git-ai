package main

import (
	"context"
	"log"
	"os"

	cli "github.com/feder-cr/git-ai/internal/cli/gitaly"
)

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}
