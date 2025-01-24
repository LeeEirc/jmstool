

BUILDDIR=build
LDFLAGS=-w -s
VERSION ?= Unknown
LDFLAGS+=-X golang.org/x/crypto/ssh.debugHandshake=true
NAME=jmstool

BUILD_INFO_GIT_TAG ?= $(shell git describe --tags 2>/dev/null || echo unknown)
BUILD_INFO_GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_INFO_BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ" || echo unknown)

all: darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows

define make_artifact_full
	@echo "Building GOOS: $(1) ARCH: $(2)"
	mkdir -p $(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/

	if [ "$(1)" = "windows" ]; then \
		CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) go build -trimpath -ldflags "${LDFLAGS}" -o $(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/$(NAME).exe .; \
	else \
		CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) go build -trimpath -ldflags "${LDFLAGS}" -o $(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/$(NAME) .; \
	fi
	cp ssh_config $(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/

	tar -C $(BUILDDIR) -czf $(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2).tar.gz $(NAME)-$(VERSION)-$(1)-$(2)/
	rm -rf $(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/
endef

darwin-amd64:
	$(call make_artifact_full,darwin,amd64)

darwin-arm64:
	$(call make_artifact_full,darwin,arm64)

linux-amd64:
	$(call make_artifact_full,linux,amd64)

linux-arm64:
	$(call make_artifact_full,linux,arm64)

windows-amd64:
	$(call make_artifact_full,windows,amd64)


.PHONY: clean
clean:
	rm -rf $(BUILDDIR)/*