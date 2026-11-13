GO ?= go
BUILDDIR ?= build
NAME := jmstool
VERSION ?= dev
BUILD_INFO_GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_INFO_BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -w -s -X github.com/LeeEirc/jmstool/cmd.Version=$(VERSION) -X github.com/LeeEirc/jmstool/cmd.Commit=$(BUILD_INFO_GIT_COMMIT) -X github.com/LeeEirc/jmstool/cmd.BuildDate=$(BUILD_INFO_BUILD_DATE)
TARGETS := darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64

.PHONY: all test vet clean $(TARGETS)

all: $(TARGETS)

define make_artifact_full
	@echo "Building $(1)/$(2)"
	mkdir -p "$(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)"
	CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/$(NAME)$(if $(filter windows,$(1)),.exe,)" .
	cp ssh_config.yml README.md LICENSE "$(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)/"
	tar -C "$(BUILDDIR)" -czf "$(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2).tar.gz" "$(NAME)-$(VERSION)-$(1)-$(2)"
	rm -rf "$(BUILDDIR)/$(NAME)-$(VERSION)-$(1)-$(2)"
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

windows-arm64:
	$(call make_artifact_full,windows,arm64)

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf "$(BUILDDIR)"
