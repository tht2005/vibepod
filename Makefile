# vibepod
#
# Two binaries: vibepod (which also answers to vp and vpinit) and vpsh.
# vpsh is separate because it is the pod's $$SHELL: it goes on the front of every
# command an agent runs, and a shell that linked in the whole client would be a
# strange thing to put there.

VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -X vibepod/internal/daemon.Version=$(VERSION)
PREFIX  ?= $(HOME)/.local

.PHONY: all build test vet clean install doctor

all: build

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/vibepod ./cmd/vibepod
	go build -o bin/vpsh ./cmd/vpsh
	@ln -sf vibepod bin/vp

# The end-to-end tests create user namespaces and run a real sshd on a high port,
# so they exercise the parts no unit test can: the kernel's cooperation, and a
# live shell on another machine.
test: build
	go test ./... -count=1

vet:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal test)" || { gofmt -l cmd internal test; exit 1; }

install: build
	install -d $(PREFIX)/bin
	install -m755 bin/vibepod $(PREFIX)/bin/vibepod
	install -m755 bin/vpsh $(PREFIX)/bin/vpsh
	ln -sf vibepod $(PREFIX)/bin/vp

doctor: build
	./bin/vibepod doctor

clean:
	rm -rf bin
