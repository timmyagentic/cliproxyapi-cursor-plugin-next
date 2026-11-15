GO ?= go
PYTHON ?= python3
GOOS := $(shell $(GO) env GOOS)
PLUGIN_ID := cursor-next
EXT := so
ifeq ($(GOOS),darwin)
EXT := dylib
endif
ifeq ($(GOOS),windows)
EXT := dll
endif

.PHONY: build test
build:
	mkdir -p dist
	$(PYTHON) scripts/release.py build --go "$(GO)" --library dist/$(PLUGIN_ID).$(EXT)

test:
	$(GO) test -race ./...
	$(GO) vet ./...
