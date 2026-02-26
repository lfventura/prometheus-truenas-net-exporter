IMAGE_NAME    := truenas-net-exporter
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Registries
GHCR_REPO     := ghcr.io/lfventura/prometheus-truenas-net-exporter
DOCKERHUB_REPO := lfventura/truenas-net-exporter

.PHONY: build push-ghcr push-dockerhub push-all login-ghcr login-dockerhub clean

## Build the Docker image
build:
	docker build -t $(IMAGE_NAME):$(VERSION) -t $(IMAGE_NAME):latest .

## Login to GHCR (needs GITHUB_TOKEN env var or will prompt)
login-ghcr:
	@echo "Logging in to ghcr.io..."
	@echo "$$GITHUB_TOKEN" | docker login ghcr.io -u lfventura --password-stdin 2>/dev/null || \
		docker login ghcr.io -u lfventura

## Login to Docker Hub
login-dockerhub:
	docker login

## Tag and push to GHCR
push-ghcr: build
	docker tag $(IMAGE_NAME):$(VERSION) $(GHCR_REPO):$(VERSION)
	docker tag $(IMAGE_NAME):latest     $(GHCR_REPO):latest
	docker push $(GHCR_REPO):$(VERSION)
	docker push $(GHCR_REPO):latest

## Tag and push to Docker Hub
push-dockerhub: build
	docker tag $(IMAGE_NAME):$(VERSION) $(DOCKERHUB_REPO):$(VERSION)
	docker tag $(IMAGE_NAME):latest     $(DOCKERHUB_REPO):latest
	docker push $(DOCKERHUB_REPO):$(VERSION)
	docker push $(DOCKERHUB_REPO):latest

## Push to both registries
push-all: push-ghcr push-dockerhub

## Remove local images
clean:
	docker rmi $(IMAGE_NAME):$(VERSION) $(IMAGE_NAME):latest 2>/dev/null || true

## Show help
help:
	@echo "Usage:"
	@echo "  make build           - Build Docker image"
	@echo "  make login-ghcr      - Login to ghcr.io"
	@echo "  make login-dockerhub - Login to Docker Hub"
	@echo "  make push-ghcr       - Push to GitHub Container Registry"
	@echo "  make push-dockerhub  - Push to Docker Hub"
	@echo "  make push-all        - Push to both registries"
	@echo "  make clean           - Remove local images"
