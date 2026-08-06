BINARY  := icloud-synk
PREFIX  ?= $(HOME)/.local
UNITDIR ?= $(HOME)/.config/systemd/user

# VERSION is stamped into the binary; defaults to the last tag plus dirty state.
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# Release targets. No CGO anywhere, so a static binary runs on any glibc/musl.
PLATFORMS := linux/amd64 linux/arm64

.PHONY: all build test vet install uninstall service clean release

all: build

build:
	go build -trimpath -ldflags='$(LDFLAGS)' -o bin/$(BINARY) ./cmd/icloud-synk

# release builds one tarball per platform plus SHA256SUMS in dist/. Asset names
# carry no version so that /releases/latest/download/<name> stays a stable URL;
# the version is in the binary itself (`icloud-synk version`).
release: test vet
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BINARY)-$$os-$$arch; \
		echo "building $$name"; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags='$(LDFLAGS)' -o dist/$$name/$(BINARY) ./cmd/icloud-synk || exit 1; \
		cp README.md README_RU.md packaging/$(BINARY).service dist/$$name/; \
		tar -czf dist/$$name.tar.gz -C dist $$name; \
		rm -rf dist/$$name; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS
	@ls -l dist

test:
	go test ./...

vet:
	go vet ./...
	gofmt -l cmd internal

install: build
	install -Dm755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "Installed $(PREFIX)/bin/$(BINARY)"

# Installs and starts the user service. Run `make install` first.
service:
	install -Dm644 packaging/$(BINARY).service $(UNITDIR)/$(BINARY).service
	systemctl --user daemon-reload
	systemctl --user enable --now $(BINARY).service
	@echo "Follow the log with: journalctl --user -u $(BINARY) -f"

uninstall:
	-systemctl --user disable --now $(BINARY).service
	-rm -f $(UNITDIR)/$(BINARY).service
	-rm -f $(PREFIX)/bin/$(BINARY)
	systemctl --user daemon-reload

clean:
	rm -rf bin
