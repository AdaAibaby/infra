ENV := $(shell cat ../../.last_used_env || echo "not-set")
-include ../../.env.${ENV}
PREFIX := $(strip $(subst ",,$(PREFIX)))

# Volcengine configuration
VOLC_REGISTRY ?= mp-bp-cn-shanghai.cr.volces.com
VOLC_NAMESPACE ?= e2b
VOLC_IMAGE_PREFIX := $(VOLC_REGISTRY)/$(VOLC_NAMESPACE)
IMAGE_TAG ?= 202615

.PHONY: build
build:
	# Allow for passing commit sha directly for docker builds
	$(eval COMMIT_SHA ?= $(shell git rev-parse --short HEAD))
	CGO_ENABLED=0 go build -o bin/docker-reverse-proxy -ldflags "-X=main.commitSHA=$(COMMIT_SHA)" .

.PHONY: build-debug
build-debug:
	CGO_ENABLED=1 go build -race -gcflags=all="-N -l" -o bin/docker-reverse-proxy .

.PHONY: build-and-upload
build-and-upload:
	$(eval COMMIT_SHA := $(shell git rev-parse --short HEAD))
	@echo "Building and pushing to Volcengine: $(VOLC_IMAGE_PREFIX)/docker-reverse-proxy:$(IMAGE_TAG)"
	@docker build \
		--tag "$(VOLC_IMAGE_PREFIX)/docker-reverse-proxy:$(IMAGE_TAG)" \
		--build-arg COMMIT_SHA="$(COMMIT_SHA)" \
		-f ./Dockerfile ..
	@echo "Pushing image to Volcengine..."
	@docker push "$(VOLC_IMAGE_PREFIX)/docker-reverse-proxy:$(IMAGE_TAG)"

.PHONY: test
test:
	go test -race -v ./...
