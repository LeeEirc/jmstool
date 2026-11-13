package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeEirc/tclientlib"
)

func startTelnetTestServer(t *testing.T, serve func(net.Conn) error) (string, string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		done <- serve(conn)
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return "admin@" + host, port, done
}

func expectTelnetBytes(conn net.Conn, want string) error {
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if string(got) != want {
		return fmt.Errorf("received %q, want %q", got, want)
	}
	return nil
}

func TestTelnetLoginAndRedirectedSession(t *testing.T) {
	for _, prompt := range []string{"READY>", "router#"} {
		t.Run(prompt, func(t *testing.T) {
			target, port, serverDone := startTelnetTestServer(t, func(conn net.Conn) error {
				if _, err := io.WriteString(conn, "Login:"); err != nil {
					return err
				}
				if err := expectTelnetBytes(conn, "admin\r\n"); err != nil {
					return err
				}
				if _, err := io.WriteString(conn, "\r\nPassword:"); err != nil {
					return err
				}
				if err := expectTelnetBytes(conn, "secret\r\n"); err != nil {
					return err
				}
				if _, err := io.WriteString(conn, "\r\n"+prompt+"\r\n"); err != nil {
					return err
				}
				data, err := io.ReadAll(conn)
				if err != nil {
					return err
				}
				// Redirected line endings are preserved; IAC is escaped by the
				// upgraded library's application-data API.
				if string(data) != "show version\r\n\xff\xffexit\n" {
					return fmt.Errorf("session input = %q", data)
				}
				// Deliver the final response only after stdin EOF. The command
				// must drain it and decode escaped IAC / NVT CR-NUL correctly.
				_, err = conn.Write([]byte("version 1.2\r\x00tail\xff\xff\r\n"))
				return err
			})
			var output, trace bytes.Buffer
			command := newTelnetCommand()
			command.SetIn(strings.NewReader("show version\r\n\xffexit\n"))
			command.SetOut(&output)
			command.SetErr(&trace)
			command.SetArgs([]string{target, "-p", port, "-P", "secret", "-c", "^READY>$", "--debug"})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := command.ExecuteContext(ctx); err != nil {
				t.Fatalf("run: %v; trace: %s", err, trace.String())
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(output.String(), "version 1.2\rtail\xff\r\n") {
				t.Fatalf("output = %q", output.String())
			}
			if !strings.Contains(trace.String(), "<redacted>") {
				t.Fatalf("expected redacted login trace: %s", trace.String())
			}
			if strings.Contains(trace.String(), "secret") || strings.Contains(trace.String(), "73 65 63 72 65 74") {
				t.Fatal("login password leaked in trace")
			}
		})
	}
}

func TestTelnetLegacyLineEndingAndExplicitEmptyPassword(t *testing.T) {
	t.Setenv("JMSTOOL_PASSWORD", "must-not-be-used")
	target, port, serverDone := startTelnetTestServer(t, func(conn net.Conn) error {
		if _, err := io.WriteString(conn, "Username:"); err != nil {
			return err
		}
		if err := expectTelnetBytes(conn, "admin\r\x00"); err != nil {
			return err
		}
		if _, err := io.WriteString(conn, "\r\nrouter#\r\n"); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	var trace bytes.Buffer
	command := newTelnetCommand()
	command.SetIn(strings.NewReader(""))
	command.SetOut(io.Discard)
	command.SetErr(&trace)
	command.SetArgs([]string{target, "-p", port, "-P", "", "--line-ending", "crnul"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if trace.Len() != 0 {
		t.Fatalf("tracing should be disabled by default: %s", trace.String())
	}
}

func TestTelnetInvalidArgumentsBeforeConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"bad regexp", []string{"-c", "["}, "invalid shell-prompt"},
		{"empty matching regexp", []string{"-c", ".*"}, "must not match an empty"},
		{"port", []string{"-p", "0"}, "port must"},
		{"timeout", []string{"--timeout", "0s"}, "timeout must"},
		{"line ending", []string{"--line-ending", "bogus"}, "invalid line ending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := newTelnetCommand()
			command.SilenceErrors = true
			command.SilenceUsage = true
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(append([]string{"admin@127.0.0.1"}, tc.args...))
			err := command.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestTelnetCancellationDuringLogin(t *testing.T) {
	accepted := make(chan struct{})
	target, port, serverDone := startTelnetTestServer(t, func(conn net.Conn) error {
		close(accepted)
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := newTelnetCommand()
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetIn(strings.NewReader(""))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{target, "-p", port, "-P", "secret", "--timeout", "30s"})
	done := make(chan error, 1)
	go func() { done <- command.ExecuteContext(ctx) }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive connection")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation failed to interrupt login")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTelnetInitialLoginTimeout(t *testing.T) {
	target, port, serverDone := startTelnetTestServer(t, func(conn net.Conn) error {
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	command := newTelnetCommand()
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetIn(strings.NewReader(""))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{target, "-p", port, "-P", "secret", "--timeout", "50ms"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := command.ExecuteContext(ctx)
	if !errors.Is(err, tclientlib.ErrLoginTimeout) {
		t.Fatalf("error = %v, want telnet login timeout", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTelnetTerminalEnterConversion(t *testing.T) {
	for _, name := range []string{"crlf", "crnul", "cr", "lf"} {
		t.Run(name, func(t *testing.T) {
			ending, err := parseTelnetLineEnding(name)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			writer := &telnetTerminalWriter{writer: &output, ending: telnetLineEndingBytes(ending)}
			for _, chunk := range []string{"one\r", "\ntwo\rthree\n\xff"} {
				if n, err := writer.Write([]byte(chunk)); n != len(chunk) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			terminator := string(telnetLineEndingBytes(ending))
			want := "one" + terminator + "two" + terminator + "three\n\xff"
			if output.String() != want {
				t.Fatalf("got %s, want %s", strconv.Quote(output.String()), strconv.Quote(want))
			}
		})
	}
}

func TestTelnetWindowChangeWireDimensions(t *testing.T) {
	peer, server := net.Pipe()
	defer peer.Close()
	defer server.Close()
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	client, err := tclientlib.NewClientConn(peer, &tclientlib.ClientConfig{Width: 120, Height: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	negotiated := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		defer server.Close()
		// IAC DO NAWS. First expect WILL NAWS and the initial 120x40 size.
		if _, err := server.Write([]byte{255, 253, 31}); err != nil {
			served <- err
			return
		}
		if err := expectTelnetBytes(server, string([]byte{255, 251, 31, 255, 250, 31, 0, 120, 0, 40, 255, 240})); err != nil {
			served <- err
			return
		}
		close(negotiated)
		// A local fallback size is 80 columns x 24 rows. Verify the wire,
		// catching the v0.0.x -> v0.1.x argument-order change despite both
		// call shapes compiling successfully.
		served <- expectTelnetBytes(server, string([]byte{255, 250, 31, 0, 80, 0, 24, 255, 240}))
	}()
	readDone := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, client); readDone <- err }()
	select {
	case <-negotiated:
	case err := <-served:
		t.Fatalf("NAWS negotiation failed: %v", err)
	}
	if err := resizeTelnetWindow(client, -1); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
}
