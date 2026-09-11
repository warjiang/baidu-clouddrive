BINARY := bdpan
OUTPUT := output
GO ?= go
VERSION ?= dev
LDFLAGS ?= -X main.version=$(VERSION)

.PHONY: build build-all darwin linux windows test clean

build:
	@mkdir -p $(OUTPUT)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY) ./cmd/bdpan

build-all: darwin linux windows

darwin:
	@mkdir -p $(OUTPUT)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY)-darwin-amd64 ./cmd/bdpan
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY)-darwin-arm64 ./cmd/bdpan

linux:
	@mkdir -p $(OUTPUT)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY)-linux-amd64 ./cmd/bdpan
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY)-linux-arm64 ./cmd/bdpan

windows:
	@mkdir -p $(OUTPUT)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY)-windows-amd64.exe ./cmd/bdpan
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUTPUT)/$(BINARY)-windows-arm64.exe ./cmd/bdpan

test:
	$(GO) test ./...

clean:
	$(GO) clean
	rm -rf $(OUTPUT)
