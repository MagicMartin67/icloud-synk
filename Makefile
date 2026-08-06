BINARY  := icloud-synk
PREFIX  ?= $(HOME)/.local
UNITDIR ?= $(HOME)/.config/systemd/user

.PHONY: all build test vet install uninstall service clean

all: build

build:
	go build -trimpath -ldflags='-s -w' -o bin/$(BINARY) ./cmd/icloud-synk

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
