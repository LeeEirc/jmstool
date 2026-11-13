package cmd

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func testSFTPClient(t *testing.T) *sftp.Client {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	server, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		_ = serverConn.Close()
		_ = clientConn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = clientConn.Close()
		_ = server.Close()
		_ = serverConn.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("SFTP server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("SFTP server did not stop")
		}
	})
	return client
}

func TestDownloadSFTPFile(t *testing.T) {
	client := testSFTPClient(t)
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "empty"},
		{name: "multiple packets", content: bytes.Repeat([]byte{0, 1, 2, '\n', 255}, 30000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := test.content
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			if err := os.WriteFile(source, content, 0600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(dir, "destination")
			n, err := downloadSFTPFile(client, filepath.ToSlash(source), destination, false)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(content)) {
				t.Fatalf("downloaded %d bytes, want %d", n, len(content))
			}
			actual, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, content) {
				t.Fatal("downloaded bytes differ from source")
			}
			assertNoPartialDownloads(t, dir)
		})
	}
}

func TestDownloadSFTPRemoteErrorsPreserveDestination(t *testing.T) {
	client := testSFTPClient(t)
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(destination, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{filepath.Join(dir, "missing"), dir} {
		if _, err := downloadSFTPFile(client, filepath.ToSlash(remote), destination, true); err == nil {
			t.Fatalf("download %q succeeded unexpectedly", remote)
		}
		assertFileContents(t, destination, "original")
		assertNoPartialDownloads(t, dir)
	}
}

func TestWriteDownloadFailurePreservesDestination(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "new"
		if force {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "destination")
			if force {
				if err := os.WriteFile(destination, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			failure := errors.New("remote connection interrupted")
			n, err := writeDownload(destination, force, func(w io.Writer) (int64, error) {
				n, err := io.WriteString(w, "partial")
				if err != nil {
					t.Fatal(err)
				}
				return int64(n), failure
			})
			if !errors.Is(err, failure) || n != 7 {
				t.Fatalf("got (%d, %v), want (7, failure)", n, err)
			}
			if force {
				assertFileContents(t, destination, "original")
			} else if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed download left destination behind: %v", err)
			}
			assertNoPartialDownloads(t, dir)
		})
	}
}

func TestWriteDownloadRejectsExistingDestination(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(destination, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := writeDownload(destination, false, func(w io.Writer) (int64, error) {
		called = true
		return io.Copy(w, strings.NewReader("new"))
	})
	if err == nil || !strings.Contains(err.Error(), "--force") || called {
		t.Fatalf("existing destination not rejected before transfer: called=%v, err=%v", called, err)
	}
	assertFileContents(t, destination, "original")
	assertNoPartialDownloads(t, dir)
}

func TestWriteDownloadNoClobberRace(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	_, err := writeDownload(destination, false, func(w io.Writer) (int64, error) {
		if err := os.WriteFile(destination, []byte("another process"), 0600); err != nil {
			t.Fatal(err)
		}
		return io.Copy(w, strings.NewReader("download"))
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("want file-exists failure, got %v", err)
	}
	assertFileContents(t, destination, "another process")
	assertNoPartialDownloads(t, dir)
}

func TestWriteDownloadForce(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(destination, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := writeDownload(destination, true, func(w io.Writer) (int64, error) {
		return io.Copy(w, strings.NewReader("complete download"))
	})
	if err != nil || n != 17 {
		t.Fatalf("got (%d, %v), want (17, nil)", n, err)
	}
	assertFileContents(t, destination, "complete download")
	assertNoPartialDownloads(t, dir)
}

func TestWriteDownloadSyncErrorPreservesDestination(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(destination, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := writeDownload(destination, true, func(w io.Writer) (int64, error) {
		file := w.(*os.File)
		n, err := io.WriteString(file, "complete but not durable")
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return int64(n), nil
	})
	if err == nil || !strings.Contains(err.Error(), "sync downloaded file") {
		t.Fatalf("want sync error, got %v", err)
	}
	assertFileContents(t, destination, "original")
	assertNoPartialDownloads(t, dir)
}

func TestWriteDownloadDoesNotFollowDestinationSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "destination")
	if err := os.Symlink(victim, destination); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	transfer := func(w io.Writer) (int64, error) { return io.Copy(w, strings.NewReader("download")) }
	if _, err := writeDownload(destination, false, transfer); err == nil {
		t.Fatal("existing symlink accepted without --force")
	}
	if _, err := writeDownload(destination, true, transfer); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, victim, "keep me")
	assertFileContents(t, destination, "download")
	info, err := os.Lstat(destination)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination is not a regular replacement: %v", err)
	}
	assertNoPartialDownloads(t, dir)
}

func TestResolveDownloadPath(t *testing.T) {
	dir := t.TempDir()
	destination, err := resolveDownloadPath("/remote/file.txt", dir, false)
	if err != nil || destination != filepath.Join(dir, "file.txt") {
		t.Fatalf("directory output: got (%q, %v)", destination, err)
	}
	for _, remote := range []string{"", "/", ".", "..", "/remote/"} {
		if _, err := resolveDownloadPath(remote, dir, false); err == nil {
			t.Errorf("remote %q accepted", remote)
		}
	}
	if _, err := resolveDownloadPath("/remote/file.txt", filepath.Join(dir, "missing", "file"), false); err == nil {
		t.Fatal("nonexistent output directory accepted")
	}
	if _, err := resolveDownloadPath(`/remote/..\elsewhere`, dir, false); err == nil {
		t.Fatal("remote name containing a Windows path separator accepted")
	}
}

func TestSFTPValidatesDownloadBeforeDialing(t *testing.T) {
	cmd := newSFTPCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"user@127.0.0.1", "--insecure", "-P", "unused"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--download is required") {
		t.Fatalf("want missing-download error, got %v", err)
	}
}

func assertNoPartialDownloads(t *testing.T, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, ".jmstool-download-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary download files remain: %v (%v)", paths, err)
	}
}

func assertFileContents(t *testing.T, filename, expected string) {
	t.Helper()
	actual, err := os.ReadFile(filename)
	if err != nil || string(actual) != expected {
		t.Fatalf("file %q: got %q (%v), want %q", filename, actual, err, expected)
	}
}
