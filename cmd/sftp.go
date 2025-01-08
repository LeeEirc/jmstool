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

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// sshCmd represents the ssh command
var sftpCmd = &cobra.Command{
	Use:   "sftp",
	Short: "JMS KoKo sftp debug tool",
	Long: `JMS KoKo sftp tool
For example:
jmstool sftp root@127.0.0.1 -p 2222 -d /tmp/file.txt
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
		if flagIdentity, err := cmd.PersistentFlags().GetString("identity"); err == nil && flagIdentity != "" {
			privateFile = flagIdentity
		}
		if flagPassword, err := cmd.PersistentFlags().GetString("password"); err == nil && flagPassword != "" {
			password = flagPassword
			auths = append(auths, gossh.Password(password))
		}
		var sshConfig SSHConfig

		defaultConfig := gossh.Config{}
		defaultConfig.SetDefaults()

		if flagConfig, err := cmd.PersistentFlags().GetString("config"); err == nil && flagConfig != "" {
			raw, err := os.ReadFile(flagConfig)
			if err != nil {
				log.Fatal(err)
			}
			if err := yaml.Unmarshal(raw, &sshConfig); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("from config: %+v\n", sshConfig)
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
			HostKeyAlgorithms: sshConfig.HostKeyAlgos,
		}
		client, err := gossh.Dial("tcp", net.JoinHostPort(host, port), config)
		if err != nil {
			log.Fatalf("dial err: %s", err)
		}
		defer client.Close()
		downloadFile := ""
		if flagDownload, err := cmd.PersistentFlags().GetString("download"); err == nil && flagDownload != "" {
			downloadFile = flagDownload

		}
		if downloadFile == "" {
			log.Println("download file is required")
			return
		}
		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			log.Printf("sftpClient failed: %s\n", err)
			return
		}

		defer sftpClient.Close()

		srcFile, err := sftpClient.Open(downloadFile)
		if err != nil {
			log.Printf("Open failed: %s\n", err)
			return
		}
		defer srcFile.Close()
		dstName := srcFile.Name()
		if strings.Contains(dstName, "/") {
			dstName = dstName[strings.LastIndex(dstName, "/")+1:]
		}
		dstFile, err := os.Create(dstName)
		if err != nil {
			log.Printf("Create failed: %s\n", err)
			return
		}
		defer dstFile.Close()
		srcReader := &fileReader{read: srcFile}
		if _, err := io.Copy(dstFile, srcReader); err != nil {
			log.Printf("Copy file failed: %s\n", err)
			return
		}
		log.Printf("Download file %s to %s success\n", downloadFile, dstName)
	},
}

func init() {
	rootCmd.AddCommand(sftpCmd)
	sftpCmd.PersistentFlags().StringP("port", "p", "22", "ssh port")
	sftpCmd.PersistentFlags().StringP("password", "P", "", "ssh password")
	sftpCmd.PersistentFlags().StringP("identity", "i", "", "identity_file")
	sftpCmd.PersistentFlags().StringP("config", "c", "", "config file for cipher, kex, hostkey, macs")
	sftpCmd.PersistentFlags().StringP("download", "d", "", "sftp download file")
	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// sshCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// sshCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

type fileReader struct {
	read io.ReadCloser
}

func (f *fileReader) Read(p []byte) (nr int, err error) {
	return f.read.Read(p)
}

func (f *fileReader) Close() error {
	return f.read.Close()
}
