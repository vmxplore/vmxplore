# Makefile — vmxplore build entry points (Phase A, in the kldload repo).
#
# Two binaries from one tree (the zxplore/wgxplore pattern):
#   vmxplore — the full build: native GUI (Fyne) + TUI (--tui). Needs cgo+GL.
#   vmx      — STATIC terminal-only build (CGO_ENABLED=0): zero runtime deps,
#              scp it to any libvirt box. Headless hosts build just this
#              (`make tui`) with nothing but the Go toolchain.
#
# BUILD dependencies (cgo + OpenGL — for the GUI binary only):
#   Fedora/RHEL:   dnf install -y golang gcc pkgconf-pkg-config \
#                    mesa-libGL-devel libX11-devel libXcursor-devel \
#                    libXrandr-devel libXinerama-devel libXi-devel \
#                    libXxf86vm-devel wayland-devel libxkbcommon-devel \
#                    fontconfig-devel
#   Debian/Ubuntu: apt-get install -y golang gcc pkg-config \
#                    libgl1-mesa-dev xorg-dev libwayland-dev \
#                    libxkbcommon-dev libfontconfig1-dev
#
# .buildnum is a local, gitignored counter that self-increments once per
# `make build` and stamps both binaries via -X main.buildNum → "0.2.2 b<N>"
# in --version and the GUI status bar. .DEFAULT_GOAL is set explicitly so
# `bump` being written first can't become the default goal (the zxplore/
# wgxplore Makefile bug, 2026-08-03).
#
# HISTORY: `install` used to depend on `build`, so the ordinary
# `make build && sudo make install` ran bump TWICE and rebuilt the binaries
# under sudo before installing them. Every build ever installed came out
# even — b34, b36, ... b44, b46 — and the odd number in between existed for
# about a second before being overwritten, which made the counter useless
# for saying "the binary you are running is the code I just changed".
# install now installs what is already in the tree; build it first.
.DEFAULT_GOAL := build

BIN_TUI := vmx
BIN_GUI := vmxplore

BUILDNUM_FILE = .buildnum
STAMPFLAGS    = -ldflags "-X main.buildNum=$$(cat $(BUILDNUM_FILE) 2>/dev/null || echo 0)"

PREFIX  ?= /usr/local
DESTDIR ?=
BINDIR   = $(DESTDIR)$(PREFIX)/bin
MANDIR   = $(DESTDIR)$(PREFIX)/share/man/man1
APPDIR   = $(DESTDIR)$(PREFIX)/share/applications
ICONDIR  = $(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps
DOCDIR   = $(DESTDIR)$(PREFIX)/share/doc/vmxplore

.PHONY: build bump tui gui test race vulncheck manlint staticcheck vet fmt check clean install uninstall

bump:
	@n=$$(cat $(BUILDNUM_FILE) 2>/dev/null || echo 0); echo $$((n + 1)) > $(BUILDNUM_FILE)

build: bump tui gui

tui:
	CGO_ENABLED=0 go build -trimpath $(STAMPFLAGS) -o $(BIN_TUI) .

gui:
	go build -trimpath -tags gui $(STAMPFLAGS) -o $(BIN_GUI) .

# GOTMPDIR is pinned inside the tree because a host that mounts /tmp noexec
# (onyx does) makes `go test` die with "fork/exec ...: permission denied" when
# it tries to run the test binary Go just linked there — a failure that reads
# like a broken test and is not one. .gotmp is gitignored.
test:
	@mkdir -p $(CURDIR)/.gotmp
	GOTMPDIR=$(CURDIR)/.gotmp go test ./...
	GOTMPDIR=$(CURDIR)/.gotmp go test -tags gui ./...

# The race detector is a SEPARATE target because it is the slow half. CI runs
# it on both flavors; so does check.
race:
	@mkdir -p $(CURDIR)/.gotmp
	GOTMPDIR=$(CURDIR)/.gotmp go test -race ./...
	GOTMPDIR=$(CURDIR)/.gotmp go test -race -tags gui ./...

# govulncheck reads the Go vulnerability database, so unlike every other gate
# here it can go red with the tree untouched -- a newly published advisory in a
# dependency is enough. That is exactly what happened on 2026-09-02: run 61 went
# red on GO-2026-6354/6355 in golang.org/x/crypto v0.54.0, reachable through
# libvirt.ConnectToURI -> ssh.Dial, on a push whose diff was six lines of ssh
# flags. Running it locally is the difference between "I broke it" and "the
# world moved".
vulncheck:
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "govulncheck NOT INSTALLED -- this check DID NOT RUN"; \
		echo "  go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

# CI fails on WARNING/ERROR and tolerates STYLE; mirror that filter exactly, or
# this passes locally and fails on the server.
manlint:
	@command -v mandoc >/dev/null 2>&1 || { \
		echo "mandoc NOT INSTALLED -- this check DID NOT RUN"; exit 1; }
	@out=$$(mandoc -T lint docs/vmxplore.1 2>&1 | grep -v ' STYLE: ' | grep -v 'outdated mandoc.db' || true); \
		if [ -n "$$out" ]; then echo "$$out"; exit 1; fi
	@echo "mandoc lint: clean"

vet:
	go vet ./...
	go vet -tags gui ./...

fmt:
	gofmt -l .

# check must run what CI runs -- ALL of it. staticcheck was added here after
# CI failed on U1000 (run 58) with a local build/vet/test/gofmt all green, and
# the comment then claimed this target matched CI. It did not: it still skipped
# the GUI-tagged tests, the race detector, govulncheck and the man page lint,
# which is four of CI's nine steps. Run 61 went red on govulncheck for that
# reason. A gate that only exists on the server is a gate you find out about
# from a red email, and a target that CLAIMS to mirror CI while skipping half
# of it is worse than one that never claimed anything.
# staticcheck as its own target so ci.yml can CALL it rather than re-typing the
# two invocations. Every gate this project has must exist exactly once, in the
# Makefile, with CI as a thin caller -- the alternative is what was here on
# 2026-09-02: four of CI's nine steps had no Makefile equivalent at all, `check`
# claimed parity it did not have, and the drift was found by a red email.
staticcheck:
	@command -v staticcheck >/dev/null 2>&1 || { \
		echo "staticcheck NOT INSTALLED — this check DID NOT RUN"; \
		echo "  go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; }
	staticcheck ./...
	staticcheck -tags gui ./...

check: vet test race build vulncheck manlint staticcheck
	@test -z "$$(gofmt -l .)" || { echo "gofmt drift:"; gofmt -l .; exit 1; }

clean:
	rm -f $(BIN_TUI) $(BIN_GUI)

install:
	@test -x $(BIN_GUI) && test -x $(BIN_TUI) || \
		{ echo "make install: build first — $(BIN_GUI)/$(BIN_TUI) not in the tree" >&2; exit 1; }
	install -d $(BINDIR) $(MANDIR) $(APPDIR) $(ICONDIR) $(DOCDIR)
	install -m 0755 $(BIN_GUI) $(BINDIR)/$(BIN_GUI)
	install -m 0755 $(BIN_TUI) $(BINDIR)/$(BIN_TUI)
	install -m 0644 docs/vmxplore.1                 $(MANDIR)/vmxplore.1
	install -m 0644 packaging/vmxplore.svg          $(ICONDIR)/vmxplore.svg
	install -m 0644 packaging/vmxplore.desktop      $(APPDIR)/vmxplore.desktop
	install -m 0644 packaging/vmxplore-tui.desktop  $(APPDIR)/vmxplore-tui.desktop
	@if [ -f README.md ]; then install -m 0644 README.md $(DOCDIR); fi
	@echo "vmxplore installed to $(PREFIX)/bin"

uninstall:
	rm -f $(BINDIR)/$(BIN_GUI) $(BINDIR)/$(BIN_TUI) \
	  $(APPDIR)/vmxplore.desktop $(APPDIR)/vmxplore-tui.desktop \
	  $(ICONDIR)/vmxplore.svg
	rm -rf $(DOCDIR)
