package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/tony1908/boxes-oss/internal/cli"
)

func main() {
	args := withAutomaticJSON(os.Args[1:], isatty.IsTerminal(os.Stdout.Fd()))
	app := cli.CLI{Interactive: isatty.IsTerminal(os.Stdin.Fd())}
	if err := app.Run(context.Background(), args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "boxes:", err)
		os.Exit(1)
	}
}

func withAutomaticJSON(args []string, stdoutIsTerminal bool) []string {
	if !stdoutIsTerminal && (len(args) == 0 || args[0] != "--json") {
		return append([]string{"--json"}, args...)
	}
	return args
}
