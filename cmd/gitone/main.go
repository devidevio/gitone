package main

import (
	"fmt"
	"os"

	"github.com/devidevio/gitone/internal/cli"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(cli.Run(os.Args[1:], cwd, os.Stdin, os.Stdout, os.Stderr))
}
