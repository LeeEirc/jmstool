package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// Split at the last @ so JumpServer selectors and email-style usernames work.
func parseTarget(target string) (username, host string, err error) {
	i := strings.LastIndexByte(target, '@')
	if i <= 0 || i == len(target)-1 || strings.IndexFunc(target, unicode.IsSpace) >= 0 || strings.IndexFunc(target, unicode.IsControl) >= 0 {
		return "", "", fmt.Errorf("invalid target %q: expected user@host", target)
	}
	username, host = target[:i], target[i+1:]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
		if addr, parseErr := netip.ParseAddr(host); parseErr != nil || !addr.Is6() {
			return "", "", fmt.Errorf("invalid IPv6 host in target %q", target)
		}
	}
	if host == "" || strings.ContainsAny(host, "/\\[]") {
		return "", "", fmt.Errorf("invalid host in target %q", target)
	}
	if strings.Contains(host, ":") {
		if addr, parseErr := netip.ParseAddr(host); parseErr != nil || !addr.Is6() {
			return "", "", fmt.Errorf("invalid host in target %q: specify the port with -p", target)
		}
	}
	return username, host, nil
}

func expandPath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~\\") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func readSecret(ctx context.Context, prompt string, errOut io.Writer) (string, error) {
	return readTerminalSecret(ctx, os.Stdin, prompt, errOut)
}

// One goroutine owns both the terminal mode and reads. The platform reader
// checks cancellation without closing stdin or leaving a blocked password
// reader that could restore stale terminal state after the command returns.
func readTerminalSecret(ctx context.Context, input *os.File, prompt string, errOut io.Writer) (secret string, err error) {
	defer runtime.KeepAlive(input)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(input.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("cannot prompt for a secret without a terminal; supply credentials using a flag or the documented environment variable")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("configure secret prompt terminal: %w", err)
	}
	defer func() {
		if restoreErr := term.Restore(fd, state); restoreErr != nil {
			secret = ""
			err = errors.Join(err, fmt.Errorf("restore secret prompt terminal: %w", restoreErr))
		}
	}()
	if _, err := fmt.Fprint(errOut, prompt); err != nil {
		return "", err
	}
	terminal := term.NewTerminal(secretReadWriter{Reader: secretInterruptReader{newSecretInput(ctx, input)}, Writer: errOut}, "")
	secret, err = terminal.ReadPassword("")
	if errors.Is(err, term.ErrPasteIndicator) {
		err = nil
	}
	if err != nil {
		fmt.Fprint(errOut, "\r\n")
		return "", fmt.Errorf("read secret: %w", err)
	}
	return secret, nil
}

type secretReadWriter struct {
	io.Reader
	io.Writer
}

type secretInterruptReader struct{ io.Reader }

func (reader secretInterruptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Do not consume input for the next prompt or shell after the first Enter.
	n, err := reader.Reader.Read(p[:1])
	if n == 1 {
		switch p[0] {
		case 3: // Raw-mode Ctrl-C does not generate SIGINT.
			return 0, context.Canceled
		case '\n':
			p[0] = '\r'
		}
	}
	return n, err
}

func terminalFD(input io.Reader) (int, bool) {
	file, ok := input.(*os.File)
	if !ok {
		return -1, false
	}
	fd := int(file.Fd())
	return fd, term.IsTerminal(fd)
}

func terminalSize(fd int) (width, height int) {
	width, height, err := term.GetSize(fd)
	if err != nil || width <= 0 || height <= 0 {
		return 80, 24
	}
	return width, height
}

func terminalType() string {
	if value := os.Getenv("TERM"); value != "" {
		return value
	}
	// Preserve the old variable as a fallback for existing debug scripts.
	if value := os.Getenv("xterm"); value != "" {
		return value
	}
	return "xterm-256color"
}
