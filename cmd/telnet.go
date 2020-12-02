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
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/LeeEirc/tclientlib"
	"golang.org/x/crypto/ssh/terminal"
)

// telnetCmd represents the telnet command
var telnetCmd = &cobra.Command{
	Use:   "telnet",
	Short: "JMS KoKo telnet tool",
	Long: `JMS KoKo telnet tool to debug
For example:
jmstool telnet root@127.0.0.1 -p 23 -P 1212
`,
	Run: func(cmd *cobra.Command, args []string) {
		var (
			username = ""
			host     = ""
			port     = "23"
			password = ""
			custom   = ""
			err      error
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

		if flagPort, err := cmd.PersistentFlags().GetString("port"); err == nil {
			port = flagPort
		}
		if flagPassword, err := cmd.PersistentFlags().GetString("password"); err == nil {
			password = flagPassword
		}
		if flagCustom, err := cmd.PersistentFlags().GetString("custom"); err == nil {
			custom = flagCustom
		}
		xterm := os.Getenv("xterm")
		if xterm == "" {
			xterm = "xterm-256color"
		}
		successRex := regexp.MustCompile(tclientlib.DefaultSuccessRegs)
		if custom != "" {
			successRex = regexp.MustCompile(custom)
		}

		fd := int(os.Stdin.Fd())
		w, h, _ := terminal.GetSize(fd)
		conf := tclientlib.Config{
			Username: username,
			Password: password,
			Timeout:  30 * time.Second,
			TTYOptions: &tclientlib.TerminalOptions{
				Wide:     w,
				High:     h,
				TermType: xterm,
			},
			LoginSuccessRegex: successRex,
		}
		client, err := tclientlib.Dial("tcp", net.JoinHostPort(host, port), &conf)
		if err != nil {
			log.Fatal(err)
		}
		state, err := terminal.MakeRaw(fd)
		if err != nil {
			log.Fatalf("MakeRaw err: %s", err)
		}
		defer terminal.Restore(fd, state)

		sigChan := make(chan struct{}, 1)

		go func() {
			sigwinchCh := make(chan os.Signal, 1)
			signal.Notify(sigwinchCh, syscall.SIGWINCH)
			for {
				select {
				case <-sigChan:
					return

				// 阻塞读取
				case sigwinch := <-sigwinchCh:
					if sigwinch == nil {
						return
					}
					w, d, err := terminal.GetSize(fd)
					if err != nil {
						log.Printf("Unable to send window-change reqest: %s. \r\n", err)
						continue
					}
					// 更新远端大小
					err = client.WindowChange(w, d)
					if err != nil {
						log.Printf("window-change err: %s\r\n", err)
						continue
					}
				}
			}
		}()
		go io.Copy(os.Stdout, client)
		io.Copy(client, os.Stdin)
		log.Println("close telnet client\r")
	},
}

func init() {
	rootCmd.AddCommand(telnetCmd)

	telnetCmd.PersistentFlags().StringP("port", "p", "23", "telnet port")
	telnetCmd.PersistentFlags().StringP("password", "P", "", "telnet password")
	telnetCmd.PersistentFlags().StringP("custom", "c", "", "custom string")
	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// telnetCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// telnetCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
