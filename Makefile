# PiQuant — build, test and install

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(DATE)

BIN     := bin
TARGET  ?= $(BIN)/piquant

.PHONY: all build test race vet fmt bench cross clean install dist package

all: fmt vet test build

## dist: build cross-compiled production binaries for Raspberry Pi 5 (arm64), Pi 3B (armv7), and x86_64
dist:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/piquant-linux-amd64 ./cmd/piquant
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/piquant-linux-arm64 ./cmd/piquant
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/piquant-linux-armv7 ./cmd/piquant
	@echo "Built release binaries in $(BIN)/"

## package: create a self-contained tarball for offline deployment
package: dist
	@tar --transform 's,^,piquant/,' -czvf $(BIN)/piquant-release.tar.gz \
		$(BIN)/piquant-linux-* deploy/ scripts/ docs/ README.md configs/
	@echo "Created deployment package: $(BIN)/piquant-release.tar.gz"

## build: native binary for this host
build:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(TARGET) ./cmd/piquant
	@echo "built $(TARGET) ($(VERSION) $(COMMIT))"

## test: unit tests for every package
test:
	CGO_ENABLED=0 $(GO) test ./...

## race: the same suite under the race detector (needed after any goroutine change)
race:
	CGO_ENABLED=0 $(GO) test -race ./...

## vet: static checks
vet:
	$(GO) vet ./...

## fmt: report unformatted files (does not rewrite)
fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

## cross: verify every supported architecture compiles
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build ./...

## bench: hot-path benchmarks (run on the target board, not the dev PC)
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/orderbook ./internal/features ./internal/models/... ./internal/telemetry

## clean
clean:
	rm -rf $(BIN)

## install: build for the Pi and install the service (run ON the board)
##   make install ROLE=dashboard   # Pi 3B
##   make install ROLE=agent       # Pi 5
ROLE ?= agent
PREFIX ?= /usr/local
install:
	@test "$(ROLE)" = "dashboard" -o "$(ROLE)" = "agent" || \
		{ echo "ROLE must be dashboard or agent"; exit 2; }
	@arch=$$(uname -m); \
	case $$arch in \
	  aarch64) goarch=arm64; goarm=;; \
	  armv7l|armv6l) goarch=arm; goarm=7;; \
	  x86_64) goarch=amd64; goarm=;; \
	  *) echo "unsupported architecture: $$arch"; exit 2;; \
	esac; \
	echo "building piquant for $$goarch$$goarm"; \
	CGO_ENABLED=0 GOOS=linux GOARCH=$$goarch GOARM=$$goarm \
		$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/piquant ./cmd/piquant
	sudo install -Dm755 $(BIN)/piquant $(PREFIX)/bin/piquant
	sudo install -Dm644 deploy/piquant-$(ROLE).service /etc/systemd/system/piquant-$(ROLE).service
	@echo "installed. Next:"
	@echo "  sudo systemctl daemon-reload"
	@echo "  piquant setup --role $(ROLE)"
	@echo "  sudo systemctl enable --now piquant-$(ROLE)"
