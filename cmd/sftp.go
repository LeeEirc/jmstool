/*
Copyright © 2020 Eric Lee

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
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"github.com/spf13/cobra"
)

func newSFTPCommand() *cobra.Command {
	var opts sshOptions
	var remote, output string
	var force bool
	cmd := &cobra.Command{
		Use:   "sftp user@host --download remote-file",
		Short: "Download a file over SFTP for connection diagnostics",
		Example: "  jmstool sftp root@127.0.0.1 -p 2222 -d /tmp/file.txt\n" +
			"  jmstool sftp root@127.0.0.1 -d /tmp/file.txt -o ./downloads/",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := parseTarget(args[0]); err != nil {
				return err
			}
			destination, err := resolveDownloadPath(remote, output, force)
			if err != nil {
				return err
			}
			client, err := dialSSH(cmd.Context(), args[0], opts, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer client.Close()
			sftpClient, err := sftp.NewClient(client)
			if err != nil {
				return fmt.Errorf("start SFTP subsystem: %w", err)
			}
			defer sftpClient.Close()
			n, err := downloadSFTPFile(sftpClient, remote, destination, force)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "Downloaded %s to %s (%d bytes)\n", remote, destination, n)
			return err
		},
	}
	addSSHFlags(cmd, &opts)
	cmd.Flags().StringVarP(&remote, "download", "d", "", "Remote file to download (required)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Local file or existing directory (default: remote file name)")
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing local file after a successful download")
	return cmd
}

func resolveDownloadPath(remote, output string, force bool) (string, error) {
	if remote == "" {
		return "", errors.New("--download is required")
	}
	name := path.Base(remote)
	if name == "." || name == ".." || name == "/" || strings.HasSuffix(remote, "/") {
		return "", fmt.Errorf("--download must name a file: %q", remote)
	}
	useRemoteName := output == ""
	if output != "" {
		var err error
		output, err = expandPath(output)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(output)
		if err == nil && info.IsDir() {
			useRemoteName = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect output %q: %w", output, err)
		}
	}
	if useRemoteName {
		// A remote POSIX name must not become a local path on Windows.
		if strings.ContainsAny(name, `/\`) || filepath.VolumeName(name) != "" {
			return "", fmt.Errorf("remote file name %q cannot be used locally; specify --output with a file name", name)
		}
		output = filepath.Join(output, name)
	}
	if err := validateDownloadDestination(output, force); err != nil {
		return "", err
	}
	return output, nil
}

func validateDownloadDestination(destination string, force bool) error {
	info, err := os.Lstat(destination)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("output %q is a directory", destination)
		}
		if !force {
			return fmt.Errorf("output %q already exists; use --force to replace it", destination)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("output %q is not a regular file", destination)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output %q: %w", destination, err)
	}
	parent, err := os.Stat(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !parent.IsDir() {
		return fmt.Errorf("output parent %q is not a directory", filepath.Dir(destination))
	}
	return nil
}

func downloadSFTPFile(client *sftp.Client, remote, destination string, force bool) (int64, error) {
	source, err := client.Open(remote)
	if err != nil {
		return 0, fmt.Errorf("open remote file %q: %w", remote, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file %q: %w", remote, err)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("remote path %q is a directory", remote)
	}
	return writeDownload(destination, force, func(destination io.Writer) (int64, error) {
		// Preserve the original tool's sequential SFTP reads for diagnostics.
		n, err := io.Copy(destination, struct{ io.Reader }{source})
		if err != nil {
			return n, fmt.Errorf("read remote file %q: %w", remote, err)
		}
		if err := source.Close(); err != nil {
			return n, fmt.Errorf("close remote file %q: %w", remote, err)
		}
		return n, nil
	})
}

// writeDownload keeps the destination untouched until the complete file has
// been written and closed. Without --force, hard-link publication also prevents
// clobbering a destination created by another process during the transfer.
func writeDownload(destination string, force bool, transfer func(io.Writer) (int64, error)) (n int64, err error) {
	if err := validateDownloadDestination(destination, force); err != nil {
		return 0, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".jmstool-download-*")
	if err != nil {
		return 0, fmt.Errorf("create temporary download file: %w", err)
	}
	defer func() {
		_ = temporary.Close()
		if removeErr := os.Remove(temporary.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			err = fmt.Errorf("remove temporary download file: %w", removeErr)
		}
	}()
	if n, err = transfer(temporary); err != nil {
		return n, fmt.Errorf("download file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return n, fmt.Errorf("sync downloaded file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return n, fmt.Errorf("close downloaded file: %w", err)
	}
	if force {
		err = os.Rename(temporary.Name(), destination)
	} else {
		err = os.Link(temporary.Name(), destination)
	}
	if err != nil {
		if !force && errors.Is(err, os.ErrExist) {
			return n, fmt.Errorf("output %q already exists; use --force to replace it: %w", destination, err)
		}
		if !force {
			return n, fmt.Errorf("publish downloaded file %q without replacement: %w; use a filesystem that supports hard links, or --force only if replacement is acceptable", destination, err)
		}
		return n, fmt.Errorf("publish downloaded file %q: %w", destination, err)
	}
	return n, nil
}
