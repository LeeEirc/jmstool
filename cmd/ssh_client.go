package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	hostkeys "github.com/skeema/knownhosts"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"gopkg.in/yaml.v3"
)

// SSHConfig preserves the algorithm configuration used by previous releases.
// Empty lists use the defaults of the linked Go SSH library.
type SSHConfig struct {
	Ciphers      []string `yaml:"Ciphers"`
	KexAlgos     []string `yaml:"KexAlgos"`
	MACs         []string `yaml:"MACs"`
	HostKeyAlgos []string `yaml:"HostKeyAlgos"`
}

type sshOptions struct {
	Port        int
	Password    string
	PasswordSet bool
	Identity    string
	ConfigFile  string
	KnownHosts  string
	Insecure    bool
	Timeout     time.Duration
}

func addSSHFlags(cmd *cobra.Command, opts *sshOptions) {
	flags := cmd.Flags()
	flags.IntVarP(&opts.Port, "port", "p", 22, "SSH port")
	flags.VarP(sshPasswordFlag{opts}, "password", "P", "SSH password (prefer JMSTOOL_PASSWORD or an interactive prompt)")
	flags.StringVarP(&opts.Identity, "identity", "i", "", "Private key file")
	flags.StringVarP(&opts.ConfigFile, "config", "c", "", "YAML file containing SSH algorithm preferences")
	flags.StringVar(&opts.KnownHosts, "known-hosts", "~/.ssh/known_hosts", "File containing trusted SSH host keys")
	flags.BoolVar(&opts.Insecure, "insecure", false, "Skip SSH host key verification for debugging")
	flags.DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Timeout for TCP connection and SSH handshake")
}

// A tracked string preserves an explicitly empty -P value over the environment.
type sshPasswordFlag struct{ opts *sshOptions }

func (flag sshPasswordFlag) String() string { return flag.opts.Password }
func (flag sshPasswordFlag) Type() string   { return "string" }
func (flag sshPasswordFlag) Set(value string) error {
	flag.opts.Password = value
	flag.opts.PasswordSet = true
	return nil
}

func loadSSHConfig(filename string) (SSHConfig, error) {
	var config SSHConfig
	if filename == "" {
		return config, nil
	}
	filename, err := expandPath(filename)
	if err != nil {
		return config, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return config, fmt.Errorf("open SSH config: %w", err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return config, fmt.Errorf("read SSH config %q: %w", filename, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return config, fmt.Errorf("read SSH config %q: %w", filename, err)
		}
		return config, fmt.Errorf("SSH config %q must contain a single YAML document", filename)
	}

	// Config.SetDefaults filters unsupported algorithms. Reject those entries
	// explicitly so that a spelling mistake cannot silently change a diagnostic.
	validated := ssh.Config{
		Ciphers:      config.Ciphers,
		KeyExchanges: config.KexAlgos,
		MACs:         config.MACs,
	}
	validated.SetDefaults()
	for _, list := range []struct {
		name      string
		requested []string
		supported []string
	}{
		{"Ciphers", config.Ciphers, validated.Ciphers},
		{"KexAlgos", config.KexAlgos, validated.KeyExchanges},
		{"MACs", config.MACs, validated.MACs},
		{"HostKeyAlgos", config.HostKeyAlgos, append(ssh.SupportedAlgorithms().HostKeys, ssh.InsecureAlgorithms().HostKeys...)},
	} {
		for _, algorithm := range list.requested {
			found := false
			for _, supported := range list.supported {
				if algorithm == supported {
					found = true
					break
				}
			}
			if !found {
				return config, fmt.Errorf("SSH config %q: unsupported %s algorithm %q", filename, list.name, algorithm)
			}
		}
	}
	return config, nil
}

func sshHostKeyCallback(opts sshOptions, address string, errOut io.Writer) (ssh.HostKeyCallback, []string, error) {
	if opts.Insecure {
		fmt.Fprintln(errOut, "Warning: SSH host key verification is disabled (--insecure).")
		return ssh.InsecureIgnoreHostKey(), nil, nil
	}
	filename := opts.KnownHosts
	if filename == "" {
		filename = "~/.ssh/known_hosts"
	}
	filename, err := expandPath(filename)
	if err != nil {
		return nil, nil, err
	}
	database, err := hostkeys.NewDB(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("read known_hosts %q: %w; add a verified host key or select a file with --known-hosts (use --insecure only for debugging)", filename, err)
	}
	check := database.HostKeyCallback()
	preferred := database.HostKeyAlgorithms(address)
	if len(preferred) != 0 {
		// Prefer keys already trusted for this host. Keep the other algorithms
		// available: a CA key and the host certificate it signs need not use the
		// same algorithm. The callback still verifies every negotiated key.
		for _, algorithm := range append(ssh.SupportedAlgorithms().HostKeys, ssh.InsecureAlgorithms().HostKeys...) {
			found := false
			for _, existing := range preferred {
				if existing == algorithm {
					found = true
					break
				}
			}
			if !found {
				preferred = append(preferred, algorithm)
			}
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyError *knownhosts.KeyError
		if errors.As(err, &keyError) {
			if len(keyError.Want) == 0 {
				return fmt.Errorf("unknown SSH host key for %s: %s %s; verify it and add it to %q: %w", hostname, key.Type(), ssh.FingerprintSHA256(key), filename, err)
			}
			return fmt.Errorf("SSH host key changed for %s: received %s %s; verify the server before updating %q: %w", hostname, key.Type(), ssh.FingerprintSHA256(key), filename, err)
		}
		return fmt.Errorf("verify SSH host key for %s: %w", hostname, err)
	}, preferred, nil
}

func loadSSHSigner(ctx context.Context, filename string, errOut io.Writer) (ssh.Signer, error) {
	filename, err := expandPath(filename)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	var missingPassphrase *ssh.PassphraseMissingError
	if !errors.As(err, &missingPassphrase) {
		if err != nil {
			return nil, fmt.Errorf("parse private key %q: %w", filename, err)
		}
		return signer, nil
	}
	passphrase, supplied := os.LookupEnv("JMSTOOL_KEY_PASSPHRASE")
	if !supplied {
		passphrase, err = readSecret(ctx, fmt.Sprintf("Passphrase for %s: ", filename), errOut)
		if err != nil {
			return nil, err
		}
	}
	signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	if err != nil {
		return nil, fmt.Errorf("decrypt private key %q: %w", filename, err)
	}
	return signer, nil
}

// isPasswordQuestion is deliberately narrow: OTP, verification-code and
// combined password/token questions must never receive the saved password.
func isPasswordQuestion(question string) bool {
	question = strings.TrimSpace(strings.ToLower(question))
	switch question {
	case "password", "password:", "password：", "密码", "密码:", "密码：":
		return true
	default:
		return false
	}
}

func sshAuthMethods(ctx context.Context, opts sshOptions, username, host string, errOut io.Writer) ([]ssh.AuthMethod, error) {
	auths := make([]ssh.AuthMethod, 0, 3)
	if opts.Identity != "" {
		signer, err := loadSSHSigner(ctx, opts.Identity, errOut)
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	password := opts.Password
	hasPassword := opts.PasswordSet || password != ""
	if !hasPassword {
		password, hasPassword = os.LookupEnv("JMSTOOL_PASSWORD")
	}
	getPassword := func() (string, error) {
		if hasPassword {
			return password, nil
		}
		var err error
		password, err = readSecret(ctx, fmt.Sprintf("%s@%s password: ", username, host), errOut)
		if err != nil {
			return "", err
		}
		hasPassword = true
		return password, nil
	}
	auths = append(auths, ssh.PasswordCallback(getPassword))
	auths = append(auths, ssh.KeyboardInteractive(sshChallenge(getPassword, func(question string) (string, error) {
		return readSecret(ctx, question+" ", errOut)
	}, errOut)))
	return auths, nil
}

func sshChallenge(getPassword func() (string, error), prompt func(string) (string, error), errOut io.Writer) ssh.KeyboardInteractiveChallenge {
	return func(user, instruction string, questions []string, echos []bool) ([]string, error) {
		if instruction != "" {
			fmt.Fprintln(errOut, instruction)
		}
		answers := make([]string, len(questions))
		for i, question := range questions {
			var err error
			if i < len(echos) && !echos[i] && isPasswordQuestion(question) {
				answers[i], err = getPassword()
			} else {
				// All other responses are entered afresh and hidden, including OTP.
				answers[i], err = prompt(question)
			}
			if err != nil {
				return nil, err
			}
		}
		return answers, nil
	}
}

// cancelSSHConn releases the context callback whenever the SSH transport closes.
// This avoids a goroutine waiting on a long-lived parent context after logout.
type cancelSSHConn struct {
	net.Conn
	stop func() bool
}

func (conn *cancelSSHConn) Close() error {
	conn.stop()
	return conn.Conn.Close()
}

func dialSSH(ctx context.Context, target string, opts sshOptions, errOut io.Writer) (*ssh.Client, error) {
	username, host, err := parseTarget(target)
	if err != nil {
		return nil, err
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return nil, fmt.Errorf("SSH port must be between 1 and 65535")
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("SSH timeout must be greater than zero")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	algorithms, err := loadSSHConfig(opts.ConfigFile)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(host, strconv.Itoa(opts.Port))
	hostKeyCallback, preferredHostKeys, err := sshHostKeyCallback(opts, address, errOut)
	if err != nil {
		return nil, err
	}
	connectCtx, stopConnecting := context.WithTimeout(ctx, opts.Timeout)
	defer stopConnecting()
	auths, err := sshAuthMethods(connectCtx, opts, username, host, errOut)
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User: username,
		Auth: auths,
		Config: ssh.Config{
			Ciphers:      nonemptyAlgorithms(algorithms.Ciphers),
			KeyExchanges: nonemptyAlgorithms(algorithms.KexAlgos),
			MACs:         nonemptyAlgorithms(algorithms.MACs),
		},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: nonemptyAlgorithms(algorithms.HostKeyAlgos),
		BannerCallback: func(message string) error {
			_, err := io.WriteString(errOut, message)
			return err
		},
	}
	if len(config.HostKeyAlgorithms) == 0 {
		config.HostKeyAlgorithms = preferredHostKeys
	}
	deadline, _ := connectCtx.Deadline()
	dialer := net.Dialer{Timeout: opts.Timeout}
	raw, err := dialer.DialContext(connectCtx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", address, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	conn := &cancelSSHConn{Conn: raw, stop: stop}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set SSH handshake deadline: %w", err)
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		if connectCtx.Err() != nil {
			return nil, fmt.Errorf("SSH connection to %s: %w", address, connectCtx.Err())
		}
		return nil, fmt.Errorf("SSH connection to %s: %w", address, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("clear SSH handshake deadline: %w", err)
	}
	if err := connectCtx.Err(); err != nil {
		_ = sshConn.Close()
		return nil, err
	}
	success = true
	return ssh.NewClient(sshConn, channels, requests), nil
}

func nonemptyAlgorithms(algorithms []string) []string {
	if len(algorithms) == 0 {
		return nil
	}
	return algorithms
}
