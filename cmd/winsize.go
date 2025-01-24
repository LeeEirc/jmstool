//go:build !windows
// +build !windows

package cmd

import (
	"os"
	"os/signal"
	"syscall"
)

func WatchWindowSize(sigwinchCh chan os.Signal) {
	signal.Notify(sigwinchCh, syscall.SIGWINCH)
}
