APP      := ova2vhdx
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
DIST     := dist

.PHONY: build test clean release all

all: test build

build:
	go build -ldflags "$(LDFLAGS)" -o $(APP) ./cmd/ova2vhdx

test:
	go test ./... -v -count=1

clean:
	rm -rf $(APP) $(APP).exe $(DIST)

# Build release binaries for all platforms
release: clean test
	@mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP)-windows-amd64.exe   ./cmd/ova2vhdx
	GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP)-windows-arm64.exe   ./cmd/ova2vhdx
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP)-linux-amd64         ./cmd/ova2vhdx
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP)-linux-arm64         ./cmd/ova2vhdx
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP)-darwin-amd64        ./cmd/ova2vhdx
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP)-darwin-arm64        ./cmd/ova2vhdx
	@echo ""
	@echo "Release binaries:"
	@ls -lh $(DIST)/
