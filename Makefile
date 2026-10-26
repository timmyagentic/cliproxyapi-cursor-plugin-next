GO ?= go
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
	CGO_ENABLED=1 $(GO) build -buildvcs=false -trimpath -buildmode=c-shared -o dist/$(PLUGIN_ID).$(EXT) .

test:
	$(GO) test -race ./...
	$(GO) vet ./...
