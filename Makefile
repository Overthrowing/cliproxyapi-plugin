GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),darwin)
EXT := dylib
else ifeq ($(GOOS),windows)
EXT := dll
else
EXT := so
endif

# The plugin ID CLIProxyAPI uses is this file's basename.
OUT := build/plugins/$(GOOS)/$(GOARCH)/litellm-quota.$(EXT)

# Shown in CLIProxyAPI's plugin list; the plugin falls back to "local" when empty.
AUTHOR ?= $(shell git log -1 --format=%an 2>/dev/null)
REPO   ?= $(patsubst %.git,%,$(shell git remote get-url origin 2>/dev/null))
LDFLAGS := -X 'main.pluginAuthor=$(AUTHOR)' -X 'main.pluginRepository=$(REPO)'

.PHONY: build test install linux clean

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) .
	rm -f $(basename $(OUT)).h

test:
	go test ./...

# make install PLUGINS_DIR=/path/to/cliproxyapi/plugins
install: build
	@test -n "$(PLUGINS_DIR)" || (echo "set PLUGINS_DIR to CLIProxyAPI's plugins.dir" && exit 1)
	mkdir -p $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)
	cp $(OUT) $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/

# Linux .so for the official Debian-based Docker image, built in a matching container.
linux:
	docker run --rm -v "$(CURDIR)":/src -w /src golang:1.26-bookworm \
		make build GOOS=linux GOARCH=$(GOARCH) AUTHOR="$(AUTHOR)" REPO="$(REPO)"

clean:
	rm -rf build
