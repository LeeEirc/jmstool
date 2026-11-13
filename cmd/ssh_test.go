package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type sshSessionResult struct {
	request string
	command string
	pty     bool
	width   uint32
	height  uint32
	err     error
}

func serveSSHSession(t *testing.T, rejectShell bool, status uint32) (string, sshOptions, <-chan sshSessionResult) {
	t.Helper()
	_, signer := testSSHKey(t)
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan sshSessionResult, 1)
	go func() {
		result := sshSessionResult{}
		defer func() { done <- result }()
		conn, err := listener.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		server, channels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			result.err = err
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		newChannel, ok := <-channels
		if !ok {
			result.err = fmt.Errorf("no SSH session channel")
			return
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer channel.Close()
		for request := range requests {
			switch request.Type {
			case "pty-req":
				var payload struct {
					Term           string
					Width, Height  uint32
					Xpixel, Ypixel uint32
					Modes          string
				}
				if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
					result.err = err
					return
				}
				result.pty, result.width, result.height = true, payload.Width, payload.Height
				_ = request.Reply(true, nil)
			case "shell", "exec":
				result.request = request.Type
				if request.Type == "exec" {
					var payload struct{ Command string }
					if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
						result.err = err
						return
					}
					result.command = payload.Command
				}
				if rejectShell {
					_ = request.Reply(false, nil)
					return
				}
				_ = request.Reply(true, nil)
				// Read until redirected stdin EOF before delivering the final output.
				if _, err := io.Copy(io.Discard, channel); err != nil {
					result.err = err
					return
				}
				_, _ = io.WriteString(channel, "remote stdout\n")
				_, _ = io.WriteString(channel.Stderr(), "remote stderr\n")
				_, result.err = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				return
			default:
				_ = request.Reply(false, nil)
			}
		}
	}()
	target, opts := sshTestOptions(t, listener.Addr().String(), signer.PublicKey())
	return target, opts, done
}

func TestSSHSessionOutputPTYAndExitStatus(t *testing.T) {
	for _, test := range []struct {
		name    string
		flags   []string
		request string
		status  uint32
		pty     bool
	}{
		{"exec", []string{"--command", "exit 7"}, "exec", 7, false},
		{"shell with redirected input", nil, "shell", 0, false},
		{"explicit PTY", []string{"--command", "printf test", "-t"}, "exec", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, opts, done := serveSSHSession(t, false, test.status)
			command := NewRootCommand()
			var output, stderr bytes.Buffer
			command.SetIn(strings.NewReader("input\n"))
			command.SetOut(&output)
			command.SetErr(&stderr)
			args := []string{"ssh", target, "-p", strconv.Itoa(opts.Port), "--known-hosts", opts.KnownHosts}
			command.SetArgs(append(args, test.flags...))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := command.ExecuteContext(ctx)
			if exitCode(err) != int(test.status) {
				t.Fatalf("exit code = %d, want %d; error: %v", exitCode(err), test.status, err)
			}
			if output.String() != "remote stdout\n" || stderr.String() != "remote stderr\n" {
				t.Fatalf("stdout=%q, stderr=%q", output.String(), stderr.String())
			}
			result := <-done
			if result.err != nil || result.request != test.request || result.pty != test.pty {
				t.Fatalf("unexpected server result: %+v", result)
			}
			if test.pty && (result.width != 80 || result.height != 24) {
				t.Fatalf("non-terminal PTY dimensions = %dx%d", result.width, result.height)
			}
			if test.request == "exec" && result.command != test.flags[1] {
				t.Fatalf("command = %q", result.command)
			}
		})
	}
}

func TestTargetParsing(t *testing.T) {
	for _, test := range []struct{ target, user, host string }{
		{"admin@host", "admin", "host"},
		{"user@example.com@jumpserver", "user@example.com", "jumpserver"},
		{"admin@account@asset@jumpserver", "admin@account@asset", "jumpserver"},
		{"admin@[2001:db8::1]", "admin", "2001:db8::1"},
		{"admin@fe80::1%eth0", "admin", "fe80::1%eth0"},
	} {
		user, host, err := parseTarget(test.target)
		if err != nil || user != test.user || host != test.host {
			t.Errorf("parseTarget(%q) = (%q,%q,%v)", test.target, user, host, err)
		}
	}
	for _, target := range []string{"host", "@host", "admin@", "admin@host:22", "admin@[host]", "admin@host/path", "admin@host\n"} {
		if _, _, err := parseTarget(target); err == nil {
			t.Errorf("accepted invalid target %q", target)
		}
	}
}
