ENV := $(shell cat ../../.last_used_env || echo "not-set")
-include ../../.env.${ENV}

LOCAL_BUILD_TARGET ?= build-debug
PREFIX := $(strip $(subst ",,$(PREFIX)))
HOSTNAME := $(shell hostname 2> /dev/null || hostnamectl hostname 2> /dev/null)
$(if $(HOSTNAME),,$(error Failed to determine hostname: both 'hostname' and 'hostnamectl' failed))

# Volcengine configuration
VOLC_REGISTRY ?= mp-bp-cn-shanghai.cr.volces.com
VOLC_NAMESPACE ?= e2b
VOLC_IMAGE_PREFIX := $(VOLC_REGISTRY)/$(VOLC_NAMESPACE)
IMAGE_TAG ?= 202621

.PHONY: build
build:
	# Allow for passing commit sha directly for docker builds
	$(eval COMMIT_SHA ?= $(shell git rev-parse --short HEAD))
	CGO_ENABLED=0 GOOS=linux GOARCH=$(BUILD_ARCH) go build -o bin/client-proxy -ldflags "-X=main.commitSHA=$(COMMIT_SHA)" .

.PHONY: build-debug
build-debug:
	$(eval COMMIT_SHA ?= $(shell git rev-parse --short HEAD))
	CGO_ENABLED=1 go build -race -gcflags=all="-N -l" -o bin/client-proxy -ldflags "-X=main.commitSHA=$(COMMIT_SHA)" .

.PHONY: build-and-upload
build-and-upload:
	$(eval COMMIT_SHA := $(shell git rev-parse --short HEAD))
	@echo "Building and pushing to Volcengine: $(VOLC_IMAGE_PREFIX)/client-proxy:$(IMAGE_TAG)"
	@docker build \
		--tag "$(VOLC_IMAGE_PREFIX)/client-proxy:$(IMAGE_TAG)" \
		--build-arg COMMIT_SHA="$(COMMIT_SHA)" \
		-f ./Dockerfile ..
	@echo "Pushing image to Volcengine..."
	@docker push "$(VOLC_IMAGE_PREFIX)/client-proxy:$(IMAGE_TAG)"

.PHONY: run
run:
	make build-debug
	NODE_ID=integration-tests \
	NODE_IP=127.0.0.1 \
	./bin/client-proxy

define setup_local_env
	$(eval include .env.local)
	$(eval export $(shell sed 's/=.*//' .env.local))
endef

.PHONY: run-local
run-local: $(LOCAL_BUILD_TARGET)
	$(call setup_local_env)
	NODE_ID=$(HOSTNAME) ./bin/client-proxy

.PHONY: generate
generate:
	go generate ./...

.PHONY: test
test:
	go test -race -v ./...
