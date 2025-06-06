IMAGE_NAME = crowlet
IMAGE_VERSION = 0.3.0
IMAGE_ORG = aleravat
IMAGE_TAG = $(IMAGE_ORG)/$(IMAGE_NAME):$(IMAGE_VERSION)
IMAGE_TAG_LATEST = $(IMAGE_ORG)/$(IMAGE_NAME):latest

.DEFAULT_GOAL := build

.PHONY: install-deps build build-static-linux test install clean docker-run docker-build docker-push docker-release

install-deps:: ## Download and installs dependencies
		@go get ./...

build:: install-deps ## Build command line binary
		@go build cmd/crowlet/crowlet.go

build-static:: install-deps ## Builds a static binary
		@CGO_ENABLED=0 \
			go build \
			-a -ldflags '-extldflags "-static"' \
				cmd/crowlet/crowlet.go

test:: ## Run tests
		@cd pkg/crawler && go test

benchmark:: ## Run benchmarks
		@cd cmd/crowlet && go test -bench=. -benchtime=30x -benchmem

install:: ## Build and install crowlet locally
		@cd cmd/crowlet/ && go install .

clean:: ## Clean build files
		@go clean cmd/crowlet/crowlet.go
		@rm crowlet

docker-run:: ## Runs the docker image
		@docker run -it --rm $(IMAGE_TAG) $(ARGS)

docker-build:: ## Builds the docker image
		@echo Building $(IMAGE_TAG)
		@docker build --pull -t $(IMAGE_TAG) .

docker-push:: ## Pushes the docker image to the registry
		@echo Pushing $(IMAGE_TAG)
		@docker image tag $(IMAGE_TAG) $(IMAGE_TAG_LATEST)
		@docker push $(IMAGE_TAG)
		@docker push $(IMAGE_TAG_LATEST)

docker-release:: docker-build docker-push ## Builds and pushes the docker image to the registry

# A help target including self-documenting targets (see the awk statement)
define HELP_TEXT
Usage: make [TARGET]... [MAKEVAR1=SOMETHING]...

Available targets:
endef
export HELP_TEXT
help: ## This help target
	@echo
	@echo "$$HELP_TEXT"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / \
		{printf "\033[36m%-30s\033[0m  %s\n", $$1, $$2}' $(MAKEFILE_LIST)
