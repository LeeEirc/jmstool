//go:build !windows

package cmd

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type secretInput struct {
	ctx context.Context
	fd  int
}

func newSecretInput(ctx context.Context, input *os.File) io.Reader {
	return &secretInput{ctx: ctx, fd: int(input.Fd())}
}

func (input *secretInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := input.ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(input.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return 0, unix.EBADF
		}
		// The prompt is the only reader of stdin, and raw mode makes a single
		// available byte sufficient to complete this read.
		n, err = unix.Read(input.fd, p[:1])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if n == 0 && err == nil {
			err = io.EOF
		}
		return n, err
	}
}
