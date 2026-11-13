package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testSSHKey(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, signer
}

// serveTestSSH owns and closes all accepted connections, even when a test
// exercises a failed handshake rather than creating a complete SSH session.
func serveTestSSH(t *testing.T, config *ssh.ServerConfig) (string, ssh.Signer) {
	t.Helper()
	_, signer := testSSHKey(t)
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	var connections []net.Conn
	stopped := false
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mutex.Lock()
			if stopped {
				mutex.Unlock()
				_ = conn.Close()
				return
			}
			connections = append(connections, conn)
			mutex.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					_ = channel.Reject(ssh.UnknownChannelType, "No channels needed by transport tests")
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mutex.Lock()
		stopped = true
		for _, conn := range connections {
			_ = conn.Close()
		}
		mutex.Unlock()
		workers.Wait()
	})
	return listener.Addr().String(), signer
}

func sshTestOptions(t *testing.T, address string, key ssh.PublicKey) (string, sshOptions) {
	t.Helper()
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	contents := ""
	if key != nil {
		contents = knownhosts.Line([]string{address}, key) + "\n"
	}
	if err := os.WriteFile(knownHosts, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return "admin@" + host, sshOptions{Port: port, Password: "test-password", KnownHosts: knownHosts, Timeout: 2 * time.Second}
}

func TestDialSSHPasswordAndCancellation(t *testing.T) {
	address, signer := serveTestSSH(t, &ssh.ServerConfig{
		PasswordCallback: func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if metadata.User() != "admin" || string(password) != "test-password" {
				return nil, errors.New("invalid credentials")
			}
			return nil, nil
		},
	})
	target, opts := sshTestOptions(t, address, signer.PublicKey())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := dialSSH(ctx, target, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not close the established SSH connection")
	}
}

func TestDialSSHRejectsUnknownAndChangedHostKeys(t *testing.T) {
	address, signer := serveTestSSH(t, &ssh.ServerConfig{NoClientAuth: true})
	_, other := testSSHKey(t)
	for _, test := range []struct {
		name string
		key  ssh.PublicKey
		want string
	}{
		{"unknown", nil, "unknown SSH host key"},
		{"changed", other.PublicKey(), "SSH host key changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, opts := sshTestOptions(t, address, test.key)
			client, err := dialSSH(context.Background(), target, opts, io.Discard)
			if client != nil {
				client.Close()
			}
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), ssh.FingerprintSHA256(signer.PublicKey())) {
				t.Fatalf("expected %q and host fingerprint, got %v", test.want, err)
			}
		})
	}
}

func TestDialSSHPrefersTrustedHostKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(ecdsaSigner)
	address, ed25519Signer := serveTestSSH(t, config)
	for _, entry := range []string{"plain", "hashed", "wildcard"} {
		t.Run(entry, func(t *testing.T) {
			target, opts := sshTestOptions(t, address, ed25519Signer.PublicKey())
			var contents string
			switch entry {
			case "hashed":
				contents = knownhosts.HashHostname(knownhosts.Normalize(address)) + " " + string(ssh.MarshalAuthorizedKey(ed25519Signer.PublicKey()))
			case "wildcard":
				contents = "* " + string(ssh.MarshalAuthorizedKey(ed25519Signer.PublicKey()))
			}
			if contents != "" {
				if err := os.WriteFile(opts.KnownHosts, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			client, err := dialSSH(context.Background(), target, opts, io.Discard)
			if err != nil {
				t.Fatalf("server's known Ed25519 key should be preferred over its untrusted ECDSA key: %v", err)
			}
			client.Close()

			// An explicitly requested diagnostic algorithm retains precedence.
			opts.ConfigFile = filepath.Join(t.TempDir(), "ssh.yml")
			if err := os.WriteFile(opts.ConfigFile, []byte("HostKeyAlgos: [ecdsa-sha2-nistp256]\n"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err = dialSSH(context.Background(), target, opts, io.Discard)
			if client != nil {
				client.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "SSH host key changed") {
				t.Fatalf("explicit HostKeyAlgos should use and reject the untrusted ECDSA key, got %v", err)
			}
		})
	}
}

func TestDialSSHHostCertificateWithDifferentCAAlgorithm(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	_, authority := testSSHKey(t)
	certificate := &ssh.Certificate{
		Key:             hostSigner.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1"},
		ValidBefore:     ssh.CertTimeInfinity,
	}
	if err := certificate.SignCert(rand.Reader, authority); err != nil {
		t.Fatal(err)
	}
	certificateSigner, err := ssh.NewCertSigner(certificate, hostSigner)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(certificateSigner)
	address, _ := serveTestSSH(t, config)
	target, opts := sshTestOptions(t, address, nil)
	trust := "@cert-authority " + knownhosts.Line([]string{address}, authority.PublicKey()) + "\n"
	if err := os.WriteFile(opts.KnownHosts, []byte(trust), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := dialSSH(context.Background(), target, opts, io.Discard)
	if err != nil {
		t.Fatalf("an ECDSA host certificate signed by a trusted Ed25519 CA should be accepted: %v", err)
	}
	client.Close()

	trust += "@revoked " + knownhosts.Line([]string{address}, authority.PublicKey()) + "\n"
	if err := os.WriteFile(opts.KnownHosts, []byte(trust), 0600); err != nil {
		t.Fatal(err)
	}
	client, err = dialSSH(context.Background(), target, opts, io.Discard)
	if client != nil {
		client.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a certificate signed by a revoked CA must be rejected, got %v", err)
	}
}

func TestDialSSHInsecureRequiresExplicitOption(t *testing.T) {
	address, _ := serveTestSSH(t, &ssh.ServerConfig{NoClientAuth: true})
	target, opts := sshTestOptions(t, address, nil)
	opts.KnownHosts = filepath.Join(t.TempDir(), "missing")
	if _, err := dialSSH(context.Background(), target, opts, io.Discard); err == nil {
		t.Fatal("missing trust file unexpectedly allowed a connection")
	}
	opts.Insecure = true
	var output bytes.Buffer
	client, err := dialSSH(context.Background(), target, opts, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if !strings.Contains(output.String(), "verification is disabled") {
		t.Fatalf("missing insecure-mode diagnostic: %s", &output)
	}
}

func TestDialSSHHandshakeTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Accept TCP but never send an SSH identification or handshake response.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
		close(accepted)
	}()
	target, opts := sshTestOptions(t, listener.Addr().String(), nil)
	opts.Insecure = true
	opts.Timeout = 100 * time.Millisecond
	start := time.Now()
	client, err := dialSSH(context.Background(), target, opts, io.Discard)
	if client != nil {
		client.Close()
	}
	_ = listener.Close()
	if conn := <-accepted; conn != nil {
		defer conn.Close()
	}
	if err == nil {
		t.Fatal("stalled SSH handshake unexpectedly succeeded")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("expected a handshake timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("handshake was not bounded by --timeout: %s", elapsed)
	}
}

func TestDialSSHEncryptedIdentity(t *testing.T) {
	key, signer := testSSHKey(t)
	address, hostSigner := serveTestSSH(t, &ssh.ServerConfig{
		PublicKeyCallback: func(metadata ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(offered.Marshal(), signer.PublicKey().Marshal()) {
				return nil, errors.New("unexpected key")
			}
			return nil, nil
		},
	})
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "test", []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(filename, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JMSTOOL_KEY_PASSPHRASE", "test-passphrase")
	target, opts := sshTestOptions(t, address, hostSigner.PublicKey())
	opts.Identity = filename
	opts.Password = ""
	// The encrypted OpenSSH key intentionally uses an expensive password KDF.
	// Match the CLI budget so race instrumentation cannot exhaust a 2s fixture.
	opts.Timeout = 30 * time.Second
	client, err := dialSSH(context.Background(), target, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	t.Setenv("JMSTOOL_KEY_PASSPHRASE", "wrong-passphrase")
	if _, err := dialSSH(context.Background(), target, opts, io.Discard); err == nil || !strings.Contains(err.Error(), "decrypt private key") {
		t.Fatalf("wrong key passphrase should fail, got %v", err)
	}
}

func TestDialSSHKeyboardInteractivePassword(t *testing.T) {
	address, signer := serveTestSSH(t, &ssh.ServerConfig{
		KeyboardInteractiveCallback: func(metadata ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := challenge(metadata.User(), "", []string{"Password:"}, []bool{false})
			if err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(answers, []string{"test-password"}) {
				return nil, errors.New("incorrect keyboard-interactive response")
			}
			return nil, nil
		},
	})
	target, opts := sshTestOptions(t, address, signer.PublicKey())
	opts.Password = ""
	t.Setenv("JMSTOOL_PASSWORD", "test-password")
	client, err := dialSSH(context.Background(), target, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func TestDialSSHEmptyAlgorithmListsUseDefaults(t *testing.T) {
	address, signer := serveTestSSH(t, &ssh.ServerConfig{NoClientAuth: true})
	target, opts := sshTestOptions(t, address, signer.PublicKey())
	opts.ConfigFile = filepath.Join(t.TempDir(), "ssh.yml")
	if err := os.WriteFile(opts.ConfigFile, []byte("Ciphers: []\nKexAlgos: []\nMACs: []\nHostKeyAlgos: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := dialSSH(context.Background(), target, opts, io.Discard)
	if err != nil {
		t.Fatalf("empty algorithm lists should successfully negotiate using defaults: %v", err)
	}
	client.Close()
}

func TestDialSSHExplicitEmptyPasswordOverridesEnvironment(t *testing.T) {
	address, signer := serveTestSSH(t, &ssh.ServerConfig{
		PasswordCallback: func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if len(password) != 0 {
				return nil, errors.New("expected an explicitly empty password")
			}
			return nil, nil
		},
	})
	target, connectionOpts := sshTestOptions(t, address, signer.PublicKey())
	var opts sshOptions
	command := &cobra.Command{}
	addSSHFlags(command, &opts)
	if err := command.ParseFlags([]string{"-P", ""}); err != nil {
		t.Fatal(err)
	}
	opts.Port = connectionOpts.Port
	opts.KnownHosts = connectionOpts.KnownHosts
	t.Setenv("JMSTOOL_PASSWORD", "must-not-be-sent")
	client, err := dialSSH(context.Background(), target, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func TestSSHChallengeDoesNotReusePasswordForOTP(t *testing.T) {
	var prompted []string
	challenge := sshChallenge(func() (string, error) { return "saved-password", nil }, func(question string) (string, error) {
		prompted = append(prompted, question)
		return "fresh-response", nil
	}, io.Discard)
	questions := []string{"Password:", "Verification code:", "Password + OTP:", "One-time password:", "Password:"}
	answers, err := challenge("admin", "MFA required", questions, []bool{false, false, false, false, true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"saved-password", "fresh-response", "fresh-response", "fresh-response", "fresh-response"}
	if !reflect.DeepEqual(answers, want) {
		t.Fatalf("unexpected challenge answers: %v", answers)
	}
	if !reflect.DeepEqual(prompted, questions[1:]) {
		t.Fatalf("unexpected prompts: %v", prompted)
	}
}

func TestLoadSSHConfig(t *testing.T) {
	for _, test := range []struct {
		name, config, wantError string
	}{
		{"defaults", "", ""},
		{"empty lists", "Ciphers: []\nMACs: []\nHostKeyAlgos: []\n", ""},
		{"valid", "Ciphers: [aes128-ctr]\nKexAlgos: [curve25519-sha256]\nMACs: [hmac-sha2-256]\n", ""},
		{"unknown field", "Cipher: [aes128-ctr]\n", "field Cipher not found"},
		{"misspelled algorithm", "KexAlgos: [urve25519-sha256]\n", "unsupported KexAlgos"},
		{"unknown host key algorithm", "HostKeyAlgos: [ssh-wrong]\n", "unsupported HostKeyAlgos"},
		{"removed DSA host key", "HostKeyAlgos: [ssh-dss]\n", "unsupported HostKeyAlgos"},
		{"wrong type", "Ciphers: aes128-ctr\n", "cannot unmarshal"},
		{"duplicate key", "Ciphers: []\nCiphers: []\n", "already defined"},
		{"multiple documents", "Ciphers: []\n---\nMACs: []\n", "single YAML document"},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "ssh.yml")
			if err := os.WriteFile(filename, []byte(test.config), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadSSHConfig(filename)
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("expected error containing %q, got %v", test.wantError, err)
			}
		})
	}
}

func TestLoadBundledSSHConfig(t *testing.T) {
	if _, err := loadSSHConfig(filepath.Join("..", "ssh_config.yml")); err != nil {
		t.Fatalf("the bundled compatibility profile must contain supported algorithms: %v", err)
	}
}

func TestDialSSHValidatesBeforeConnecting(t *testing.T) {
	for _, opts := range []sshOptions{
		{Port: 0, Timeout: time.Second},
		{Port: 65536, Timeout: time.Second},
		{Port: 22, Timeout: 0},
	} {
		t.Run(fmt.Sprint(opts.Port, "/", opts.Timeout), func(t *testing.T) {
			if _, err := dialSSH(context.Background(), "admin@127.0.0.1", opts, io.Discard); err == nil {
				t.Fatal("invalid options were accepted")
			}
		})
	}
}
