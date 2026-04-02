REGISTRY=registry.skull.everyof.net
IMAGE=$(REGISTRY)/mcpeto
BUILD_DIR=./build
BUILD=$(shell git rev-parse --short HEAD)@$(shell date +%s)
CURRENT_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
CURRENT_ARCH := $(shell uname -m | tr '[:upper:]' '[:lower:]')
LD_FLAGS=-ldflags "-X main.BuildVersion=$(BUILD)"
GO_BUILD=CGO_ENABLED=0 go build $(LD_FLAGS)

.PHONY: build
build:
	$(GO_BUILD) -o $(BUILD_DIR)/ ./...

.PHONY: test
test:
	go test -count=1 -timeout 60s ./...

.PHONY: buildLinuxX86
buildLinuxX86:
	GOOS=linux GOARCH=amd64 $(GO_BUILD) -o $(BUILD_DIR)/ ./...

# Build and push multi-platform image (amd64 first — cluster is amd64).
.PHONY: image
image:
	docker buildx build \
		--platform=linux/amd64,linux/arm64 \
		-t $(IMAGE):$(shell git rev-parse --short HEAD) \
		-t $(IMAGE):latest \
		. --push --provenance=false

# Build amd64-only image (faster, for iteration).
.PHONY: image-amd64
image-amd64:
	docker buildx build \
		--platform=linux/amd64 \
		-t $(IMAGE):$(shell git rev-parse --short HEAD) \
		-t $(IMAGE):latest \
		. --push --provenance=false

.PHONY: format
format:
	go fix ./...
	go fmt ./...
	go vet ./...
	go get ./...
	go test ./...
	go mod tidy
	golangci-lint fmt --no-config --enable gofmt,goimports
	golangci-lint run --no-config --fix
	nilaway -include-pkgs="$(MODULE)" ./...

.PHONY: helm-lint
helm-lint:
	helm lint charts/mcpeto

.PHONY: helm-template
helm-template:
	helm template mcpeto charts/mcpeto
