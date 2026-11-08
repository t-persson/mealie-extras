export GOCACHE := $(CURDIR)/.cache/go-build
export GOPATH := $(CURDIR)/.cache/go-path
export GOTMPDIR := $(CURDIR)/.cache/tmp

.PHONY: build test vet fmt

build:
	@mkdir -p bin "$(GOTMPDIR)"
	go build -o bin/mealie-generate ./cmd/mealie-generate
	go build -o bin/mealie-shoppinglist ./cmd/mealie-shoppinglist

test:
	@mkdir -p "$(GOTMPDIR)"
	go test ./...

vet:
	@mkdir -p "$(GOTMPDIR)"
	go vet ./...

fmt:
	gofmt -w cmd internal
