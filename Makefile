BINARY  := filestore
PKG     := ./cmd/filestore
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

PLATFORMS := darwin/arm64 darwin/amd64 windows/amd64 linux/amd64 linux/arm64

.PHONY: build all dist install test vet clean

build:
	go build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

# One binary per platform, in bin/
all:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=bin/$(BINARY)-$$os-$$arch; \
		[ $$os = windows ] && out=$$out.exe; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done
	@ls -lh bin/

# Release archives with checksums, in dist/
dist: clean-dist
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BINARY)-$(VERSION)-$$os-$$arch; \
		mkdir -p dist/$$name; \
		bin=dist/$$name/$(BINARY); \
		[ $$os = windows ] && bin=$$bin.exe; \
		GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" -o $$bin $(PKG) || exit 1; \
		cp README.md LICENSE dist/$$name/; \
		if [ $$os = windows ]; then \
			(cd dist && zip -qr $$name.zip $$name); \
		else \
			(cd dist && tar czf $$name.tar.gz $$name); \
		fi; \
		rm -rf dist/$$name; \
	done
	@cd dist && shasum -a 256 * > SHA256SUMS 2>/dev/null || sha256sum * > SHA256SUMS
	@ls -lh dist/

install: build
	install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

clean-dist:
	rm -rf dist/

clean: clean-dist
	rm -rf bin/
