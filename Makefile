BINARY  := mkdocsgo
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The MkDocs project used by the run targets.
PROJECT ?= .
ADDR    ?= 127.0.0.1:8080

.PHONY: all build test lint serve site mcp stdio anchors image clean

all: test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/anchorcheck ./cmd/anchorcheck

test:
	go test ./...

lint:
	gofmt -l . | tee /dev/stderr | wc -l | grep -qx 0
	go vet ./...

# Site and MCP from one process, which is how it is deployed.
serve: build
	./bin/$(BINARY) -mode site+mcp -project $(PROJECT) -http $(ADDR)

# Static site only - the nginx replacement.
site: build
	./bin/$(BINARY) -mode site -project $(PROJECT) -http $(ADDR)

# MCP only, over HTTP.
mcp: build
	./bin/$(BINARY) -mode mcp -project $(PROJECT) -http $(ADDR)

# MCP only, over stdio - what a local MCP client launches.
stdio: build
	./bin/$(BINARY) -mode mcp -project $(PROJECT)

# Compare computed anchors against a built site. Requires `mkdocs build` first.
anchors: build
	./bin/anchorcheck -project $(PROJECT) -site $(PROJECT)/site

image:
	docker build -t $(BINARY):$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -rf bin dist
