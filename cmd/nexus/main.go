// nexus 命令行入口（F5.4）：serve / agent 子命令。
package main

import (
	"fmt"
	"os"
)

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "nexus:", err)
		os.Exit(1)
	}
}
