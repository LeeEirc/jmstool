package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func secretTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo-terminals unavailable: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

type secretPromptOutput struct {
	bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (out *secretPromptOutput) Write(p []byte) (int, error) {
	n, err := out.Buffer.Write(p)
	if len(p) != 0 {
		out.once.Do(func() { close(out.ready) })
	}
	return n, err
}

func TestSecretPromptRestoresTerminal(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
		cancel            bool
		wantError         error
	}{
		{name: "Unicode and backspace", input: "p密x\x7f码🔒\r", want: "p密码🔒"},
		{name: "Ctrl-C", input: "partial\x03", wantError: context.Canceled},
		{name: "Ctrl-D", input: "\x04", wantError: io.EOF},
		{name: "external cancellation", cancel: true, wantError: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			master, slave := secretTestPTY(t)
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := &secretPromptOutput{ready: make(chan struct{})}
			type result struct {
				secret string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				secret, err := readTerminalSecret(ctx, slave, "Password: ", out)
				done <- result{secret, err}
			}()
			select {
			case <-out.ready:
			case <-ctx.Done():
				t.Fatal("secret prompt did not start")
			}
			if test.cancel {
				cancel()
			} else if _, err := io.WriteString(master, test.input); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.secret != test.want || !errors.Is(got.err, test.wantError) {
					t.Fatalf("got secret %q, error %v; want %q, %v", got.secret, got.err, test.want, test.wantError)
				}
				if test.wantError == context.Canceled && exitCode(got.err) != 130 {
					t.Fatalf("canceled prompt exit code = %d, want 130", exitCode(got.err))
				}
			case <-time.After(time.Second):
				t.Fatal("secret prompt did not return promptly")
			}
			after, err := term.GetState(int(slave.Fd()))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("secret prompt did not restore terminal: %v", err)
			}
			if !strings.HasPrefix(out.String(), "Password: ") || strings.Contains(out.String(), "密") || strings.Contains(out.String(), "partial") {
				t.Fatalf("secret prompt output is missing or exposed input: %q", out.String())
			}
			// Restoration must leave stdin open, with no background reader
			// consuming the next command's input.
			if _, err := io.WriteString(master, "next command\n"); err != nil {
				t.Fatal(err)
			}
			fds := []unix.PollFd{{Fd: int32(slave.Fd()), Events: unix.POLLIN}}
			if n, err := unix.Poll(fds, 500); err != nil || n != 1 {
				t.Fatalf("terminal input is not available after prompt: %d, %v", n, err)
			}
			var input [64]byte
			n, err := unix.Read(int(slave.Fd()), input[:])
			if err != nil || string(input[:n]) != "next command\n" {
				t.Fatalf("subsequent input was consumed or stdin closed: %q (%v)", input[:n], err)
			}
		})
	}
}

func TestSecretPromptPreservesNextResponse(t *testing.T) {
	master, slave := secretTestPTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := &secretPromptOutput{ready: make(chan struct{})}
	type result struct {
		first, second string
		err           error
	}
	done := make(chan result, 1)
	go func() {
		first, err := readTerminalSecret(ctx, slave, "Password: ", out)
		if err != nil {
			done <- result{err: err}
			return
		}
		second, err := readTerminalSecret(ctx, slave, "Code: ", out)
		done <- result{first, second, err}
	}()
	select {
	case <-out.ready:
	case <-ctx.Done():
		t.Fatal("secret prompt did not start")
	}
	if _, err := io.WriteString(master, "first\rsecond\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.first != "first" || got.second != "second" {
			t.Fatalf("consecutive prompt responses: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("consecutive prompts did not finish")
	}
}

func TestSecretPromptDeadlineAndOutputFailureRestoreTerminal(t *testing.T) {
	_, slave := secretTestPTY(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := readTerminalSecret(ctx, slave, "Password: ", io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline did not stop prompt: %v", err)
	}
	if _, err := readTerminalSecret(context.Background(), slave, "Password: ", secretErrorWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output failure was not returned: %v", err)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal not restored on failure: %v", err)
	}
}

type secretErrorWriter struct{}

func (secretErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
