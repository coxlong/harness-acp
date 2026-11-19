// Command harnessd is the daemon for the harness-acp project.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/coxlong/harness-acp/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "harnessd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("harnessd starting...")
	// TODO: implement Agent Client Protocol (ACP) logic.
	return nil
}
