# vibepod
#
# Two binaries: vibepod (which also answers to vpctl and vpinit) and vpsh.
# vpsh is separate because a shim is never invoked under its own name — the
# kernel reaches it through a bind mount, and argv[0] is whatever the caller
# passed — so it identifies itself by /proc/self/exe instead.

VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -X vibepod/internal/daemon.Version=$(VERSION)
PREFIX  ?= $(HOME)/.local

.PHONY: all build test vet clean install doctor

all: build

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/vibepod ./cmd/vibepod
	go build -o bin/vpsh ./cmd/vpsh
	@ln -sf vibepod bin/vpctl

# The end-to-end tests create user namespaces, install seccomp filters and run
# a real sshd on a high port, so they exercise the parts no unit test can.
test: build
	go test ./... -count=1

vet:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal test)" || { gofmt -l cmd internal test; exit 1; }

install: build
	install -d $(PREFIX)/bin
	install -m755 bin/vibepod $(PREFIX)/bin/vibepod
	install -m755 bin/vpsh $(PREFIX)/bin/vpsh
	ln -sf vibepod $(PREFIX)/bin/vpctl

doctor: build
	./bin/vpctl doctor

clean:
	rm -rf bin
