/*
Copyright © 2020 Eric Lee

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LeeEirc/tclientlib"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type telnetOptions struct {
	port       int
	password   string
	custom     string
	timeout    time.Duration
	debug      bool
	lineEnding string
}

func newTelnetCommand() *cobra.Command {
	var opts telnetOptions
	command := &cobra.Command{
		Use:     "telnet user@host",
		Short:   "Diagnose a Telnet connection",
		Long:    "Connect with automatic login. --custom adds a shell-prompt expression to the built-in matchers.\nTelnet sends credentials and session data without encryption.",
		Example: "  jmstool telnet admin@192.0.2.10\n  jmstool telnet admin@192.0.2.10 -c '^READY>$' --line-ending crnul",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTelnet(cmd, args[0], opts)
		},
	}
	flags := command.Flags()
	flags.IntVarP(&opts.port, "port", "p", 23, "Telnet port")
	flags.StringVarP(&opts.password, "password", "P", "", "Password (prefer prompt or JMSTOOL_PASSWORD; empty only supports servers that do not request a password)")
	flags.StringVarP(&opts.custom, "custom", "c", "", "Additional final shell-prompt regular expression")
	flags.DurationVar(&opts.timeout, "timeout", 30*time.Second, "Timeout for connection and initial login")
	flags.BoolVar(&opts.debug, "debug", false, "Write protocol tracing to stderr (may include post-login session data)")
	flags.StringVar(&opts.lineEnding, "line-ending", "crlf", "Login and interactive Enter terminator: crlf, crnul, cr, or lf")
	return command
}

func runTelnet(cmd *cobra.Command, target string, opts telnetOptions) error {
	user, host, err := parseTarget(target)
	if err != nil {
		return err
	}
	if opts.port < 1 || opts.port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	ending, err := parseTelnetLineEnding(opts.lineEnding)
	if err != nil {
		return err
	}
	var shellPrompt *regexp.Regexp
	if opts.custom != "" {
		shellPrompt, err = regexp.Compile(opts.custom)
		if err != nil {
			return fmt.Errorf("invalid shell-prompt expression: %w", err)
		}
		if shellPrompt.MatchString("") {
			return fmt.Errorf("shell-prompt expression must not match an empty string")
		}
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	if !cmd.Flags().Changed("password") {
		var supplied bool
		opts.password, supplied = os.LookupEnv("JMSTOOL_PASSWORD")
		if !supplied {
			opts.password, err = readSecret(cmd.Context(), fmt.Sprintf("%s password: ", target), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
		}
	}

	input := cmd.InOrStdin()
	fd, isTerminal := terminalFD(input)
	width, height := 80, 24
	if isTerminal {
		width, height = terminalSize(fd)
	}
	config := &tclientlib.ClientConfig{
		User:         user,
		Password:     opts.password,
		Timeout:      opts.timeout,
		TerminalType: terminalType(),
		Width:        width,
		Height:       height,
		ShellPrompt:  shellPrompt,
		LineEnding:   ending,
	}
	if opts.debug {
		config.Trace = cmd.ErrOrStderr()
	}

	// NewClientConn allows context cancellation to interrupt both initial login
	// and the established session. A single budget covers TCP and initial login.
	deadline := time.Now().Add(opts.timeout)
	dialer := net.Dialer{Timeout: opts.timeout}
	conn, err := dialer.DialContext(cmd.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(opts.port)))
	if err != nil {
		return fmt.Errorf("connect Telnet: %w", err)
	}
	defer conn.Close()
	stopCancellation := context.AfterFunc(cmd.Context(), func() { _ = conn.Close() })
	defer stopCancellation()
	config.Timeout = time.Until(deadline)
	if config.Timeout <= 0 {
		return fmt.Errorf("connect Telnet: %w", context.DeadlineExceeded)
	}
	client, err := tclientlib.NewClientConn(conn, config)
	if err != nil {
		if ctxErr := cmd.Context().Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("login Telnet: %w", err)
	}
	defer client.Close()
	if err := cmd.Context().Err(); err != nil {
		return err
	}

	var inputWriter io.Writer = client
	resize := make(chan os.Signal, 1)
	if isTerminal {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("configure terminal: %w", err)
		}
		defer term.Restore(fd, state)
		WatchWindowSize(resize)
		defer signal.Stop(resize)
		inputWriter = &telnetTerminalWriter{writer: client, ending: telnetLineEndingBytes(ending)}
	}

	outputDone := make(chan error, 1)
	inputDone := make(chan error, 1)
	outputComplete := false
	defer func() {
		_ = client.Close()
		// Finish all remote output before restoring the terminal or returning
		// to a caller that may inspect/reuse its output writer.
		if !outputComplete {
			<-outputDone
		}
	}()
	go func() {
		_, err := io.Copy(cmd.OutOrStdout(), client)
		outputDone <- err
	}()
	go func(done chan<- error) {
		_, err := io.Copy(inputWriter, input)
		if err == nil {
			// Keep reading after redirected stdin ends so the final response is
			// not truncated. TCP EOF lets servers decide when to finish.
			if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
				err = halfCloser.CloseWrite()
			}
		}
		done <- err
	}(inputDone)
	for {
		select {
		case err := <-outputDone:
			outputComplete = true
			if ctxErr := cmd.Context().Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				return fmt.Errorf("read Telnet: %w", err)
			}
			return nil
		case err := <-inputDone:
			inputDone = nil
			if err != nil {
				if ctxErr := cmd.Context().Err(); ctxErr != nil {
					return ctxErr
				}
				return fmt.Errorf("write Telnet: %w", err)
			}
		case <-resize:
			if err := resizeTelnetWindow(client, fd); err != nil {
				return fmt.Errorf("resize Telnet terminal: %w", err)
			}
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		}
	}
}

// Terminal size APIs return width first; tclientlib v0.1.x follows SSH and
// expects height first. Keep the adaptation in one place.
func resizeTelnetWindow(client *tclientlib.Client, fd int) error {
	width, height := terminalSize(fd)
	return client.WindowChange(height, width)
}

func parseTelnetLineEnding(value string) (tclientlib.LineEnding, error) {
	switch strings.ToLower(value) {
	case "crlf":
		return tclientlib.LineEndingCRLF, nil
	case "crnul":
		return tclientlib.LineEndingCRNUL, nil
	case "cr":
		return tclientlib.LineEndingCR, nil
	case "lf":
		return tclientlib.LineEndingLF, nil
	default:
		return 0, fmt.Errorf("invalid line ending %q: use crlf, crnul, cr, or lf", value)
	}
}

func telnetLineEndingBytes(ending tclientlib.LineEnding) []byte {
	switch ending {
	case tclientlib.LineEndingCRNUL:
		return []byte{'\r', 0}
	case tclientlib.LineEndingCR:
		return []byte{'\r'}
	case tclientlib.LineEndingLF:
		return []byte{'\n'}
	default:
		return []byte{'\r', '\n'}
	}
}

// telnetTerminalWriter maps raw terminal Enter keys to the selected terminator.
// Redirected input bypasses it, preserving caller-supplied data byte for byte.
// A pasted CRLF is one newline even if its bytes arrive in different writes.
type telnetTerminalWriter struct {
	writer  io.Writer
	ending  []byte
	afterCR bool
}

func (w *telnetTerminalWriter) Write(p []byte) (int, error) {
	data := make([]byte, 0, len(p))
	for _, b := range p {
		if w.afterCR && b == '\n' {
			w.afterCR = false
			continue
		}
		w.afterCR = b == '\r'
		if b == '\r' {
			data = append(data, w.ending...)
		} else {
			data = append(data, b)
		}
	}
	n, err := w.writer.Write(data)
	if err != nil {
		return 0, err
	}
	if n != len(data) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}
