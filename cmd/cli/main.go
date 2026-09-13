package main

import (
	"fmt"
	"os"

	"github.com/xiws/otter/pkg/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}
