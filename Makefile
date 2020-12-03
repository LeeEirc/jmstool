
BUILD_INFO_GIT_TAG ?= $(shell git describe --tags 2>/dev/null || echo unknown)
BUILD_INFO_GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_INFO_BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ" || echo unknown)

release: linux darwin windows

.PHONY: linux
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -x -ldflags "-X golang.org/x/crypto/ssh.debugHandshake=true" -o  jmstool_linux .

.PHONY: darwin
darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -x -ldflags "-X golang.org/x/crypto/ssh.debugHandshake=true" -o jmstool_darwin .

.PHONY: windows
windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -x -ldflags "-X golang.org/x/crypto/ssh.debugHandshake=true" -o jmstool_windows.exe .


.PHONY: clean
clean:
	rm -f jmstool_linux jmstool_darwin jmstool_windows.exe