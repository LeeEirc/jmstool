

.PHONY: linux
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o jmstool_linux .

.PHONY: darwin
darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o jmstool_darwin .
