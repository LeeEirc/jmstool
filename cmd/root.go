// Copyright 2020 Eric. Licensed under the Apache License, Version 2.0.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// Build information is populated by the Makefile's linker flags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// NewRootCommand creates independent commands so flags do not leak between runs.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "jmstool",
		Short:         "Diagnose SSH, SFTP and Telnet connections to JumpServer and assets",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, BuildDate),
	}
	root.SetIn(os.Stdin)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.AddCommand(newSSHCommand(), newSFTPCommand(), newTelnetCommand())
	return root
}

func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := NewRootCommand().ExecuteContext(ctx)
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var remoteExit *ssh.ExitError
	if errors.As(err, &remoteExit) {
		if status := remoteExit.ExitStatus(); status > 0 && status < 256 {
			return status
		}
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}
