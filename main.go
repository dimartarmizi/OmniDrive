package main

import (
	"os"

	"omnidrive/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
