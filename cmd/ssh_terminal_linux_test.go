package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Exercise the failure path with a real local pseudo-terminal: log.Fatal used
// to skip the terminal restoration deferred by the SSH command.
func TestSSHRestoresTerminalWhenShellFails(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pseudo-terminals unavailable: %v", err)
	}
	defer master.Close()
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
	defer slave.Close()
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	target, opts, done := serveSSHSession(t, true, 0)
	command := NewRootCommand()
	command.SetIn(slave)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"ssh", target, "-p", strconv.Itoa(opts.Port), "--known-hosts", opts.KnownHosts})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := command.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), "could not start shell") {
		t.Fatalf("expected rejected shell, got %v", err)
	}
	result := <-done
	if result.err != nil || !result.pty {
		t.Fatalf("server result: %+v", result)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("terminal was not restored after shell rejection")
	}
}
