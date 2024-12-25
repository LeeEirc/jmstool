/*
Copyright © 2020 NAME HERE <EMAIL ADDRESS>

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
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

var (
	supportedCiphers = []string{
		"aes128-ctr", "aes192-ctr", "aes256-ctr",
		"aes128-gcm@openssh.com",
		"chacha20-poly1305@openssh.com",
		"arcfour256", "arcfour128", "arcfour",
		"aes128-cbc",
		"3des-cbc"}

	supportedKexAlgos = []string{
		"diffie-hellman-group1-sha1",
		"diffie-hellman-group14-sha1", "ecdh-sha2-nistp256", "ecdh-sha2-nistp521",
		"ecdh-sha2-nistp384", "curve25519-sha256@libssh.org"}

	supportedHostKeyAlgos = []string{
		"ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"ssh-rsa", "ssh-dss",
		"ssh-ed25519",
	}
)

// old ssh ciphers issue
// https://github.com/golang/go/issues/64779

type SSHConfig struct {
	Ciphers      []string `yaml:"Ciphers"`
	KexAlgos     []string `yaml:"KexAlgos"`
	MACs         []string `yaml:"MACs"`
	HostKeyAlgos []string `yaml:"HostKeyAlgos"`
}

// sshCmd represents the ssh command
var sshCmd = &cobra.Command{
	Use:   "ssh",
	Short: "JMS KoKo ssh debug tool",
	Long: `JMS KoKo ssh tool
For example:
jmstool ssh root@127.0.0.1 -p 2222
`,
	Run: func(cmd *cobra.Command, args []string) {
		var (
			username    = ""
			host        = ""
			port        = "2222"
			privateFile = ""
			password    = ""
		)
		for i := range args {
			if strings.Contains(args[i], "@") {
				usernameHost := strings.Split(args[i], "@")
				if len(usernameHost) != 2 {
					fmt.Println("error format: ", args[i])
					os.Exit(1)
				}
				username, host = usernameHost[0], usernameHost[1]
				break
			}
		}
		if username == "" || host == "" {
			_ = cmd.Help()
			os.Exit(1)
		}
		auths := make([]gossh.AuthMethod, 0, 2)

		if flagPort, err := cmd.PersistentFlags().GetString("port"); err == nil {
			port = flagPort
		}
		if flagIdentity, err := cmd.PersistentFlags().GetString("identity"); err == nil {
			privateFile = flagIdentity
		}
		if flagPassword, err := cmd.PersistentFlags().GetString("password"); err == nil {
			password = flagPassword
			auths = append(auths, gossh.Password(password))
		}
		var sshConfig SSHConfig

		defaultConfig := ssh.Config{}
		defaultConfig.SetDefaults()

		if flagConfig, err := cmd.PersistentFlags().GetString("config"); err == nil {
			raw, err := os.ReadFile(flagConfig)
			if err != nil {
				log.Fatal(err)
			}
			if err := yaml.Unmarshal(raw, &sshConfig); err != nil {
				log.Fatal(err)
			}
			if len(sshConfig.Ciphers) == 0 {
				sshConfig.Ciphers = defaultConfig.Ciphers
			}
			if len(sshConfig.KexAlgos) == 0 {
				sshConfig.KexAlgos = defaultConfig.KeyExchanges
			}
			if len(sshConfig.HostKeyAlgos) == 0 {
				sshConfig.HostKeyAlgos = nil
			}
			if len(sshConfig.MACs) == 0 {
				sshConfig.MACs = defaultConfig.MACs
			}

		}

		if password == "" && privateFile == "" {
			if _, err := fmt.Fprintf(os.Stdout, "%s@%s password: ", username, host); err != nil {
				log.Fatal(err)
			}
			if result, err := term.ReadPassword(int(os.Stdout.Fd())); err != nil {
				log.Fatal(err)
			} else {
				auths = append(auths, gossh.Password(string(result)))
			}

		}
		if privateFile != "" {
			raw, err := os.ReadFile(privateFile)
			if err != nil {
				log.Fatal(err)
			}
			signer, err := gossh.ParsePrivateKey(raw)
			if err != nil {
				log.Fatal(err)
			}

			auths = append(auths, gossh.PublicKeys(signer))
		}

		config := &gossh.ClientConfig{
			User:              username,
			Auth:              auths,
			HostKeyCallback:   gossh.InsecureIgnoreHostKey(),
			Config:            gossh.Config{Ciphers: sshConfig.Ciphers, KeyExchanges: sshConfig.KexAlgos, MACs: sshConfig.MACs},
			Timeout:           30 * time.Second,
			HostKeyAlgorithms: supportedHostKeyAlgos,
		}
		client, err := gossh.Dial("tcp", net.JoinHostPort(host, port), config)
		if err != nil {
			log.Fatalf("dial err: %s", err)
		}
		defer client.Close()
		sess, err := client.NewSession()
		if err != nil {
			log.Fatalf("Session err: %s", err)
		}
		modes := gossh.TerminalModes{
			gossh.ECHO:          1,     // enable echoing
			gossh.TTY_OP_ISPEED: 14400, // input speed = 14.4 kbaud
			gossh.TTY_OP_OSPEED: 14400, // output speed = 14.4 kbaud
		}
		xterm := os.Getenv("xterm")
		if xterm == "" {
			xterm = "xterm-256color"
		}
		fd := int(os.Stdin.Fd())
		w, h, _ := term.GetSize(fd)
		err = sess.RequestPty(xterm, h, w, modes)
		if err != nil {
			log.Fatalf("RequestPty err: %s", err)
		}
		in, err := sess.StdinPipe()
		if err != nil {
			log.Fatalf("StdinPipe err: %s", err)
		}
		out, err := sess.StdoutPipe()
		if err != nil {
			log.Fatalf("StdoutPipe err: %s", err)
		}
		state, err := term.MakeRaw(fd)
		if err != nil {
			log.Fatalf("MakeRaw err: %s", err)
		}
		defer term.Restore(fd, state)

		go io.Copy(in, os.Stdin)
		go io.Copy(os.Stdout, out)
		sigwinchCh := make(chan os.Signal, 1)
		WatchWindowSize(sigwinchCh)
		sigChan := make(chan struct{}, 1)
		err = sess.Shell()
		if err != nil {
			log.Fatalf("Shell err: %s", err)
		}
		go func() {
			for {
				select {
				case <-sigChan:
					return

				// 阻塞读取
				case sigwinch := <-sigwinchCh:
					if sigwinch == nil {
						return
					}
					w, h, err := term.GetSize(fd)
					if err != nil {
						log.Printf("Unable to send window-change reqest: %s. \n", err)
						continue
					}
					if err := sess.WindowChange(h, w); err != nil {
						log.Println("Window change err: ", err)
					}
				}
			}
		}()
		err = sess.Wait()
		sigChan <- struct{}{}
		if err != nil {
			log.Fatalf("Wait err: %s", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(sshCmd)
	sshCmd.PersistentFlags().StringP("port", "p", "22", "ssh port")
	sshCmd.PersistentFlags().StringP("password", "P", "", "ssh password")
	sshCmd.PersistentFlags().StringP("identity", "i", "", "identity_file")
	sshCmd.PersistentFlags().StringP("config", "c", "", "config file for cipher, kex, hostkey, macs")
	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// sshCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// sshCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
