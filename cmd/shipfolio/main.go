// Command shipfolio reads a Shipfolio workspace from the terminal.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/cli"
)

func main() {
	// Release builds set the version with -ldflags; "go install …@v1.2.3"
	// records it in the build info instead.
	if bi, ok := debug.ReadBuildInfo(); ok && cli.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		cli.Version = bi.Main.Version
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	app := &cli.App{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Getenv:      os.Getenv,
		Interactive: interactive,
		ReadSecret:  readSecret,
	}
	os.Exit(app.Run(ctx, os.Args[1:]))
}

func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// readSecret turns terminal echo off with stty where it exists. Where it
// does not (Windows), the key is echoed; pipe it in instead.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	stty := func(arg string) error {
		cmd := exec.Command("stty", arg)
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}
	if stty("-echo") == nil {
		defer func() {
			_ = stty("echo")
			fmt.Fprintln(os.Stderr)
		}()
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return line, nil
}
