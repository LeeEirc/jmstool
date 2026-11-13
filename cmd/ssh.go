// Copyright 2020 Eric. Licensed under the Apache License, Version 2.0.
package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func newSSHCommand() *cobra.Command {
	var opts sshOptions
	var command string
	var forcePTY, noPTY bool
	cmd := &cobra.Command{
		Use:     "ssh user@host",
		Short:   "Open an SSH shell or run a remote command",
		Example: "  jmstool ssh admin@jumpserver.example.com -p 2222\n  jmstool ssh root@asset.example.com --command 'uname -a'",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("command") && command == "" {
				return fmt.Errorf("--command must not be empty")
			}
			client, err := dialSSH(cmd.Context(), args[0], opts, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer client.Close()
			session, err := client.NewSession()
			if err != nil {
				return fmt.Errorf("open SSH session: %w", err)
			}
			defer session.Close()
			session.Stdin = cmd.InOrStdin()
			session.Stdout = cmd.OutOrStdout()
			session.Stderr = cmd.ErrOrStderr()

			fd, isTerminal := terminalFD(cmd.InOrStdin())
			usePTY := forcePTY || (!noPTY && command == "" && isTerminal)
			if usePTY {
				width, height := terminalSize(fd)
				modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
				if err := session.RequestPty(terminalType(), height, width, modes); err != nil {
					return fmt.Errorf("request SSH terminal: %w", err)
				}
				if isTerminal {
					state, err := term.MakeRaw(fd)
					if err != nil {
						return fmt.Errorf("configure local terminal: %w", err)
					}
					defer term.Restore(fd, state)
					changes := make(chan os.Signal, 1)
					done := make(chan struct{})
					stopped := make(chan struct{})
					WatchWindowSize(changes)
					defer func() {
						signal.Stop(changes)
						close(done)
						// Unblock an outstanding resize write before joining the watcher.
						client.Close()
						<-stopped
					}()
					go func() {
						defer close(stopped)
						for {
							select {
							case <-done:
								return
							case <-changes:
								width, height := terminalSize(fd)
								if err := session.WindowChange(height, width); err != nil {
									return
								}
							}
						}
					}()
				}
			}
			if command != "" {
				err = session.Run(command)
			} else if err = session.Shell(); err == nil {
				err = session.Wait()
			}
			if cmd.Context().Err() != nil {
				return cmd.Context().Err()
			}
			if err != nil {
				return fmt.Errorf("SSH session: %w", err)
			}
			return nil
		},
	}
	addSSHFlags(cmd, &opts)
	cmd.Flags().StringVarP(&command, "command", "e", "", "run one remote command without a PTY by default")
	cmd.Flags().BoolVarP(&forcePTY, "pty", "t", false, "request a PTY even for a remote command or redirected input")
	cmd.Flags().BoolVarP(&noPTY, "no-pty", "T", false, "do not request a PTY")
	cmd.MarkFlagsMutuallyExclusive("pty", "no-pty")
	return cmd
}
