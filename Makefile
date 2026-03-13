.SILENT:
.PHONY: proto build test lint clean manifests docker-build docker-push docker-compose-up docker-compose-down \
        minikube-build k8s-deploy k8s-delete k8s-delete-all k8s-status help

DOCKER_REPO   ?= ghcr.io/pg-swarm
IMAGE_TAG     ?= latest
DOCKERFILE_DIR = deploy/docker
PLATFORMS     ?= linux/amd64,linux/arm64

help: ## Show this help
	@printf "\nUsage: make <target>\n"
	@printf "\nVariables:\n"
	@printf "  DOCKER_REPO   Registry + org prefix  (default: ghcr.io/pg-swarm)\n"
	@printf "  IMAGE_TAG     Image tag               (default: latest)\n"
	@printf "\nTargets:\n"
	@grep -E '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | \
		sed 's/:.*## /\t/' | \
		awk -F'\t' '{ printf "  %-22s %s\n", $$1, $$2 }'
	@printf "\n"

proto: ## Generate Go code from .proto files (requires buf)
	buf generate

build: proto ## Compile central, satellite, and failover-sidecar binaries into bin/
	go build -o bin/central ./cmd/central
	go build -o bin/satellite ./cmd/satellite
	go build -o bin/failover-sidecar ./cmd/failover-sidecar

test: ## Run all Go tests
	go test ./...

manifests: ## Regenerate operator manifest YAMLs in testdata/ for inspection
	go test ./internal/satellite/operator/ -run TestManifests -count=1

lint: ## Run golangci-lint
	golangci-lint run ./...

clean: ## Remove compiled binaries and generated proto code
	rm -rf bin/ api/gen/v1/*.go

# ── Docker ────────────────────────────────────────────────────────────────────

docker-build: ## Build multi-platform images ($(PLATFORMS)) locally (no push, no load)
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.central \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) .
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) .
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.failover-sidecar \
		-t $(DOCKER_REPO)/pg-swarm-failover:$(IMAGE_TAG) .

docker-push: ## Build multi-platform images ($(PLATFORMS)) and push to DOCKER_REPO
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.central \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) \
		--push .
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) \
		--push .
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.failover-sidecar \
		-t $(DOCKER_REPO)/pg-swarm-failover:$(IMAGE_TAG) \
		--push .

docker-compose-up: ## Build and start the full stack (postgres + central + satellite)
	docker compose -f $(DOCKERFILE_DIR)/docker-compose.yml up --build -d

docker-compose-down: ## Stop the stack and remove volumes
	docker compose -f $(DOCKERFILE_DIR)/docker-compose.yml down -v

# ── Minikube / Kubernetes ─────────────────────────────────────────────────────

MINIKUBE_ARCH ?= $(shell minikube ssh "uname -m" 2>/dev/null | tr -d '\r' | sed 's/x86_64/amd64/;s/aarch64/arm64/')

minikube-build: ## Build for minikube's arch (linux/$$MINIKUBE_ARCH) and load into its daemon
	eval $$(minikube docker-env) && \
	docker buildx build --platform linux/$(MINIKUBE_ARCH) \
		-f $(DOCKERFILE_DIR)/Dockerfile.central \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) --load . && \
	docker buildx build --platform linux/$(MINIKUBE_ARCH) \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) --load . && \
	docker buildx build --platform linux/$(MINIKUBE_ARCH) \
		-f $(DOCKERFILE_DIR)/Dockerfile.failover-sidecar \
		-t $(DOCKER_REPO)/pg-swarm-failover:$(IMAGE_TAG) --load .

k8s-deploy: ## Apply all manifests (postgres + app) to the pgswarm-system namespace
	kubectl apply -f deploy/k8s/namespace.yaml
	kubectl apply -k deploy/k8s/postgres/
	kubectl apply -k deploy/k8s

k8s-delete: ## Remove app resources (central + satellite). Postgres is intentionally preserved.
	kubectl delete -k deploy/k8s/

k8s-delete-all: ## Remove everything including postgres and its data (destructive)
	kubectl delete -k deploy/k8s/
	kubectl delete -k deploy/k8s/postgres/
	kubectl delete -f deploy/k8s/namespace.yaml

k8s-status: ## Show all resources in the pgswarm-system namespace
	kubectl get all -n pgswarm-system
