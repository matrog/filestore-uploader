BINARY := filestore
PKG    := ./cmd/filestore

.PHONY: build all clean install test

build:
	go build -ldflags="-s -w" -o bin/$(BINARY) $(PKG)

# Binari per tutte le piattaforme, in bin/
all: clean
	GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o bin/$(BINARY)-macos-arm64 $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o bin/$(BINARY)-macos-intel $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/$(BINARY)-windows.exe $(PKG)
	GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o bin/$(BINARY)-linux $(PKG)
	@ls -lh bin/

install: build
	install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)

test:
	go test ./...

clean:
	rm -rf bin/
