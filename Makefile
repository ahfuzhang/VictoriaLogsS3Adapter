BUILD_DIR   ?= build
GO          ?= go
GOFLAGS     ?= -trimpath
LDFLAGS     ?= -s -w
CGO_ENABLED ?= 0

DOCKER             ?= docker
IMAGE              ?= victoria-logs-s3-adapter
TAG                ?= latest
DOCKER_BUILD_FLAGS ?=

BINARIES  := victoria_logs_s3_backup victoria_logs_s3_restore_svr
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build cross linux $(PLATFORMS) docker test vet fmt clean help

all: build

## build: build binaries for the host platform into $(BUILD_DIR)/
build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/ ./cmd/...

## linux: build linux/amd64 and linux/arm64 into $(BUILD_DIR)/<os>_<arch>/
linux: linux/amd64 linux/arm64

## cross: build all platforms in PLATFORMS into $(BUILD_DIR)/<os>_<arch>/
cross: $(PLATFORMS)

$(PLATFORMS):
	@mkdir -p $(BUILD_DIR)/$(subst /,_,$@)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(word 1,$(subst /, ,$@)) GOARCH=$(word 2,$(subst /, ,$@)) \
		$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(subst /,_,$@)/ ./cmd/...

## docker: build Docker image $(IMAGE):$(TAG); binaries are compiled inside the image
##         (e.g. make docker TAG=v1.0.0 DOCKER_BUILD_FLAGS='--platform linux/amd64')
# make docker DOCKER_BUILD_FLAGS='--platform linux/amd64' IMAGE=ahfuzhang/victoria-logs-s3-adapter TAG=v0.3.0
docker:
	$(DOCKER) build $(DOCKER_BUILD_FLAGS) -t $(IMAGE):$(TAG) .

## test: run unit tests
test:
	$(GO) test ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: format source code
fmt:
	$(GO) fmt ./...

## clean: remove $(BUILD_DIR)/
clean:
	rm -rf $(BUILD_DIR)

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
