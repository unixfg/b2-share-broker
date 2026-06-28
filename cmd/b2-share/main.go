package main

import (
	"context"
	"fmt"
	"os"

	"github.com/unixfg/b2-share-broker/internal/sharecli"
)

func main() {
	if err := sharecli.NewCommand().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
