.SILENT:
.PHONY: proto dashboard dashboard-dev dashboard-mock build test test-integration lint clean manifests \
        docker-build-central docker-build-satellite docker-build-sentinel docker-build-all \
        docker-push-central docker-push-satellite docker-push-sentinel docker-push-all \
        docker-compose-up docker-compose-down \
        minikube-build-central minikube-build-satellite minikube-build-sentinel minikube-build-all \
        k8s-deploy-central k8s-deploy-central-minikube k8s-deploy-satellite k8s-deploy-satellite-minikube k8s-deploy-all \
        k8s-delete-central k8s-delete-satellite k8s-delete-all \
        k8s-status k8s-refresh-deploy help

DOCKER_REPO    ?= ghcr.io/pg-swarm
IMAGE_TAG      ?= latest
DOCKERFILE_DIR  = deploy/docker
PLATFORMS      ?= linux/amd64,linux/arm64

help: ## Show this help
	@printf "\nUsage: make <target>\n"
	@printf "\nVariables:\n"
	@printf "  DOCKER_REPO   Registry + org prefix  (default: ghcr.io/pg-swarm)\n"
	@printf "  IMAGE_TAG     Image tag               (default: latest)\n"
	@printf "\nTargets:\n"
	@grep -E '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) | \
		sed 's/:.*## /\t/' | \
		awk -F'\t' '{ printf "  %-28s %s\n", $$1, $$2 }'
	@printf "\n"

# ── Build ────────────────────────────────────────────────────────────────────

proto: ## Generate Go code from .proto files (requires buf)
	buf generate

dashboard: ## Build the React dashboard into dashboard/static/
	cd dashboard && npm install && npm run build

dashboard-dev: ## Run React dashboard with hot-reload (proxies API to localhost:8080)
	cd dashboard && npm install && npm run dev

dashboard-mock: ## Run React dashboard with mock data (no backend needed)
	cd dashboard && npm install && MOCK=true npm run dev

build: proto dashboard ## Compile central, satellite, and sentinel-sidecar binaries
	go build -o bin/central ./cmd/central
	go build -o bin/satellite ./cmd/satellite
	go build -o bin/sentinel-sidecar ./cmd/sentinel-sidecar

clean: ## Remove compiled binaries and generated proto code
	rm -rf bin/ api/gen/v1/*.go

# ── Test ─────────────────────────────────────────────────────────────────────

test: ## Run unit tests
	go test ./...

test-integration: ## Run integration tests against minikube (requires running cluster)
	go test -tags integration -timeout 10m -v ./internal/satellite/operator/

test-e2e: ## Run end-to-end chaos tests (requires minikube with images loaded)
	go test -tags e2e -timeout 30m -v ./test/e2e/

manifests: ## Regenerate operator manifest YAMLs in testdata/
	go test ./internal/satellite/operator/ -run TestManifests -count=1

lint: ## Run golangci-lint
	golangci-lint run ./...

# ── Docker (multi-platform) ──────────────────────────────────────────────────

docker-build-central: ## Build central image (multi-platform, no push)
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.central \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) .

docker-build-satellite: ## Build satellite image (multi-platform, no push)
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) .

docker-build-sentinel: ## Build sentinel-sidecar image (multi-platform, no push)
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.sentinel-sidecar \
		-t $(DOCKER_REPO)/pg-swarm-sentinel:$(IMAGE_TAG) .

docker-build-all: docker-build-central docker-build-satellite docker-build-sentinel ## Build all images (multi-platform, no push)

docker-push-central: ## Build and push central image
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.central \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) --push .

docker-push-satellite: ## Build and push satellite image
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) --push .

docker-push-sentinel: ## Build and push sentinel-sidecar image
	docker buildx build --platform $(PLATFORMS) \
		-f $(DOCKERFILE_DIR)/Dockerfile.sentinel-sidecar \
		-t $(DOCKER_REPO)/pg-swarm-sentinel:$(IMAGE_TAG) --push .

docker-push-all: docker-push-central docker-push-satellite docker-push-sentinel ## Build and push all images

docker-compose-up: ## Build and start the full stack (postgres + central + satellite)
	docker compose -f $(DOCKERFILE_DIR)/docker-compose.yml up --build -d

docker-compose-down: ## Stop the stack and remove volumes
	docker compose -f $(DOCKERFILE_DIR)/docker-compose.yml down -v

# ── Minikube ─────────────────────────────────────────────────────────────────

MINIKUBE_ARCH ?= $(shell minikube ssh "uname -m" 2>/dev/null | tr -d '\r' | sed 's/x86_64/amd64/;s/aarch64/arm64/')

minikube-build-central: ## Build central image and load into minikube
	eval $$(minikube docker-env) && \
	docker buildx build --platform linux/$(MINIKUBE_ARCH) \
		-f $(DOCKERFILE_DIR)/Dockerfile.central \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) --load .

minikube-build-satellite: ## Build satellite image and load into minikube
	eval $$(minikube docker-env) && \
	docker buildx build --platform linux/$(MINIKUBE_ARCH) \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) --load .

minikube-build-sentinel: ## Build sentinel-sidecar image and load into minikube
	eval $$(minikube docker-env) && \
	docker buildx build --platform linux/$(MINIKUBE_ARCH) \
		-f $(DOCKERFILE_DIR)/Dockerfile.sentinel-sidecar \
		-t $(DOCKER_REPO)/pg-swarm-sentinel:$(IMAGE_TAG) --load .

minikube-build-all: minikube-build-central minikube-build-satellite minikube-build-sentinel ## Build all images and load into minikube

# ── Kubernetes ───────────────────────────────────────────────────────────────

k8s-deploy-central: ## Deploy postgres + central to pgswarm-system namespace
	kubectl apply -k deploy/k8s/central/base/

k8s-deploy-central-minikube: ## Deploy postgres + central to pgswarm-system-central namespace
	kubectl apply -k deploy/k8s/central/overlays/minikube/

k8s-deploy-satellite: ## Deploy satellite to pgswarm-system namespace
	kubectl apply -k deploy/k8s/satellite/base/

k8s-deploy-satellite-minikube: ## Deploy satellite with CENTRAL_ADDR pointing to host (for local central)
	kubectl apply -k deploy/k8s/satellite/overlays/minikube/

k8s-deploy-all: k8s-deploy-central k8s-deploy-satellite ## Deploy central (with postgres) + satellite

k8s-delete-central: ## Remove central + postgres resources
	kubectl delete -k deploy/k8s/central/base/

k8s-delete-satellite: ## Remove satellite resources
	kubectl delete -k deploy/k8s/satellite/base/

k8s-delete-all: ## Remove everything (destructive)
	-kubectl delete -k deploy/k8s/satellite/base/
	-kubectl delete -k deploy/k8s/central/base/

k8s-refresh-deploy: minikube-build-all k8s-deploy-all ## Rebuild images in minikube and redeploy all

k8s-status: ## Show all resources in the pgswarm-system namespace
	kubectl get all -n pgswarm-system
