BIN     := librescoot-flasher
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := CGO_ENABLED=0

.PHONY: all linux darwin windows arm clean

all: linux darwin windows arm

linux:
	$(GOFLAGS) GOOS=linux GOARCH=amd64  go build -ldflags="$(LDFLAGS)" -o $(BIN)-linux-amd64  .
	$(GOFLAGS) GOOS=linux GOARCH=arm64  go build -ldflags="$(LDFLAGS)" -o $(BIN)-linux-arm64  .

arm:
	$(GOFLAGS) GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="$(LDFLAGS)" -o $(BIN)-linux-arm .

darwin:
	$(GOFLAGS) GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(BIN)-darwin-arm64 .

windows:
	$(GOFLAGS) GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BIN)-windows-amd64.exe .

clean:
	rm -f $(BIN) $(BIN).exe $(BIN)-linux-* $(BIN)-darwin-* $(BIN)-windows-*
