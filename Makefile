SHELL=/bin/bash -e -o pipefail
PWD = $(shell pwd)
ROOT_DIR = $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))

BIN_SUBDIRS := cmd/ecsnode cmd/etcdcluster
# Build configuration
GCFLAGS ?=
LDFLAGS ?= -w -s
BUILD_PLATFORMS ?= linux/amd64

# Docker configuration
DOCKER_REGISTRY ?=
DOCKER_IMAGE ?= etcdauto
DOCKER_TAG ?= latest

# Full image name
ifeq ($(DOCKER_REGISTRY),)
DOCKER_FULL_IMAGE = $(DOCKER_IMAGE):$(DOCKER_TAG)
else
DOCKER_FULL_IMAGE = $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG)
endif

# Build flags
GO_BUILD_FLAGS = -gcflags="$(GCFLAGS)" -ldflags="$(LDFLAGS)"
GO_BUILD = go build $(GO_BUILD_FLAGS)

all: git-hooks  tidy ## Initializes all tools

out:
	@mkdir -p out

git-hooks:
	@git config --local core.hooksPath .githooks/

download: ## Downloads the dependencies
	@go mod download

tidy: ## Cleans up go.mod and go.sum
	@go mod tidy

fmt: ## Formats all code with go fmt
	@go fmt ./...

test-build: ## Tests whether the code compiles
	@go build -o /dev/null ./...

build: 
	@for DIR in $(BIN_SUBDIRS); do \
		for PLATFORM in $(BUILD_PLATFORMS); do \
			mkdir -p $(ROOT_DIR)/bin/$${PLATFORM}; \
			echo "Building \"$${DIR##*/}\" for $${PLATFORM}"; \
			GOOS=$${PLATFORM%/*} GOARCH=$${PLATFORM#*/} \
			$(GO_BUILD) -o $(ROOT_DIR)/bin/$${PLATFORM} $(ROOT_DIR)/$$DIR; \
		done; \
	done
	@echo "Build complete."

lint: fmt tidy download ## Lints all code with golangci-lint
	@go run github.com/golangci/golangci-lint/cmd/golangci-lint run

lint-reports: out/lint.xml

.PHONY: out/lint.xml
out/lint.xml: out download
	@go run github.com/golangci/golangci-lint/cmd/golangci-lint run ./... --out-format checkstyle | tee "$(@)"

govulncheck: ## Vulnerability detection using govulncheck
	@go run golang.org/x/vuln/cmd/govulncheck ./...

test: ## Runs all tests
	@go test $(ARGS) ./...

coverage: out/report.json ## Displays coverage per func on cli
	go tool cover -func=out/cover.out

html-coverage: out/report.json ## Displays the coverage results in the browser
	go tool cover -html=out/cover.out

test-reports: out/report.json

.PHONY: out/report.json
out/report.json: out
	@go test -count 1 ./... -coverprofile=out/cover.out --json | tee "$(@)"

clean: ## Cleans up everything
	@rm -rf bin out

docker: ## Builds docker image
	@echo "Building Docker image: $(DOCKER_FULL_IMAGE)"
	docker buildx build --load --cache-to type=inline  -t $(DOCKER_FULL_IMAGE) .

docker-push: docker ## Builds and pushes docker image
	@echo "Pushing Docker image: $(DOCKER_FULL_IMAGE)"
	docker push $(DOCKER_FULL_IMAGE)

generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	@go run sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

define make-go-dependency
  # target template for go tools, can be referenced e.g. via /bin/<tool>
  bin/$(notdir $1):
	GOBIN=$(PWD)/bin go install $1
endef

# this creates a target for each go dependency to be referenced in other targets
$(foreach dep, $(GO_DEPENDENCIES), $(eval $(call make-go-dependency, $(dep))))
ci: lint-reports test-reports govulncheck ## Executes vulnerability scan, lint, test and generates reports

help: ## Shows the help
	@echo 'Usage: make <OPTIONS> ... <TARGETS>'
	@echo ''
	@echo 'Available targets are:'
	@echo ''
	@grep -E '^[ a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
        awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ''
	@echo 'Build configuration variables:'
	@echo '  GCFLAGS          Go compiler flags (default: empty)'
	@echo '  LDFLAGS          Linker flags (default: -w -s)'
	@echo '  PLATFORMS        Target platforms (default: linux/amd64)'
	@echo '                   Example: linux/amd64,linux/arm64,darwin/amd64'
	@echo ''
	@echo 'Docker configuration variables:'
	@echo '  DOCKER_REGISTRY  Docker registry (default: empty)'
	@echo '  DOCKER_IMAGE     Docker image name (default: etcdauto)'
	@echo '  DOCKER_TAG       Docker image tag (default: latest)'
	@echo ''
	@echo 'Examples:'
	@echo '  make build LDFLAGS="-X main.Version=v1.0.0"'
	@echo '  make build-multiplatform PLATFORMS=linux/amd64,linux/arm64'
	@echo '  make docker DOCKER_REGISTRY=myregistry.io DOCKER_TAG=v1.0.0'
	@echo ''
