.SILENT:
.PHONY: proto dashboard build build-debug test test-integration lint clean manifests \
        docker-build docker-push docker-compose-up docker-compose-down \
        minikube-build minikube-debug k8s-deploy k8s-delete k8s-delete-all k8s-refresh-deploy k8s-status \
        local-pg local-central local-satellite local-stop help

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
		awk -F'\t' '{ printf "  %-22s %s\n", $$1, $$2 }'
	@printf "\n"

# ── Build ────────────────────────────────────────────────────────────────────

proto: ## Generate Go code from .proto files (requires buf)
	buf generate

dashboard: ## Build the React dashboard into web/static/
	cd web/dashboard && npm install && npm run build

build: proto dashboard ## Compile central, satellite, and failover-sidecar binaries
	go build -o bin/central ./cmd/central
	go build -o bin/satellite ./cmd/satellite
	go build -o bin/failover-sidecar ./cmd/failover-sidecar

clean: ## Remove compiled binaries and generated proto code
	rm -rf bin/ api/gen/v1/*.go

# ── Test ─────────────────────────────────────────────────────────────────────

test: ## Run unit tests
	go test ./...

test-integration: ## Run integration tests against minikube (requires running cluster)
	go test -tags=integration ./internal/satellite/operator/ -run TestIntegration -v -count=1 -timeout=10m

manifests: ## Regenerate operator manifest YAMLs in testdata/
	go test ./internal/satellite/operator/ -run TestManifests -count=1

lint: ## Run golangci-lint
	golangci-lint run ./...

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

build-debug: proto dashboard ## Compile debug binaries (no optimisation, for dlv) into bin/
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MINIKUBE_ARCH) \
		go build -gcflags="all=-N -l" -o bin/central ./cmd/central
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MINIKUBE_ARCH) \
		go build -gcflags="all=-N -l" -o bin/satellite ./cmd/satellite
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MINIKUBE_ARCH) \
		go build -gcflags="all=-N -l" -o bin/failover-sidecar ./cmd/failover-sidecar

minikube-debug: build-debug proto dashboard## Build debug images with dlv and load into minikube
	eval $$(minikube docker-env) && \
	docker build \
		-f $(DOCKERFILE_DIR)/Dockerfile.central.debug \
		-t $(DOCKER_REPO)/pg-swarm-central:$(IMAGE_TAG) --load . && \
	docker build \
		-f $(DOCKERFILE_DIR)/Dockerfile.satellite.debug \
		-t $(DOCKER_REPO)/pg-swarm-satellite:$(IMAGE_TAG) --load . && \
	docker build \
		-f $(DOCKERFILE_DIR)/Dockerfile.failover-sidecar.debug \
		-t $(DOCKER_REPO)/pg-swarm-failover:$(IMAGE_TAG) --load .
	@echo ""
	@echo "Debug images loaded into minikube. dlv ports:"
	@echo "  central:          40000"
	@echo "  satellite:        40001"
	@echo "  failover-sidecar: 40002"
	@echo ""
	@echo "Connect:  dlv connect <minikube-ip>:<port>"
	@echo "Or:       kubectl port-forward <pod> 40000:40000"

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

k8s-refresh-deploy: minikube-debug k8s-deploy ## Delete app, rebuild images in minikube, and redeploy

k8s-delete-all: ## Remove everything including postgres and its data (destructive)
	kubectl delete -k deploy/k8s/
	kubectl delete -k deploy/k8s/postgres/
	kubectl delete -f deploy/k8s/namespace.yaml

k8s-status: ## Show all resources in the pgswarm-system namespace
	kubectl get all -n pgswarm-system

# ── Local dev (binaries on host, postgres in minikube) ────────────────────────

LOCAL_PG_PORT  ?= 5433
LOCAL_PID_DIR  := .run

local-pg: ## Port-forward minikube postgres to localhost:$(LOCAL_PG_PORT) (background)
	@mkdir -p $(LOCAL_PID_DIR)
	@if [ -f $(LOCAL_PID_DIR)/pg-forward.pid ] && kill -0 $$(cat $(LOCAL_PID_DIR)/pg-forward.pid) 2>/dev/null; then \
		echo "port-forward already running (pid $$(cat $(LOCAL_PID_DIR)/pg-forward.pid))"; \
	else \
		kubectl port-forward -n pgswarm-system svc/postgres $(LOCAL_PG_PORT):5432 & \
		echo $$! > $(LOCAL_PID_DIR)/pg-forward.pid; \
		echo "postgres port-forward started on localhost:$(LOCAL_PG_PORT) (pid $$!)"; \
		sleep 1; \
	fi

local-central: local-pg build ## Build and run central server locally
	@mkdir -p $(LOCAL_PID_DIR)
	PG_HOST=localhost PG_PORT=$(LOCAL_PG_PORT) PG_USER=pgswarm PG_PASSWORD=pgswarm PG_DB=pgswarm PG_SSL_MODE=disable \
		GRPC_ADDR=":9090" HTTP_ADDR=":8080" \
		./bin/central & \
	echo $$! > $(LOCAL_PID_DIR)/central.pid; \
	echo "central started (pid $$!): http://localhost:8080  grpc://localhost:9090"

local-satellite: build ## Build and run satellite locally (connects to local central, operates on minikube)
	@mkdir -p $(LOCAL_PID_DIR)
	CENTRAL_ADDR=localhost:9090 K8S_CLUSTER_NAME=minikube REGION=local \
		DEPLOY_NAMESPACE=default \
		./bin/satellite & \
	echo $$! > $(LOCAL_PID_DIR)/satellite.pid; \
	echo "satellite started (pid $$!)"

local-dashboard: ## Run React dashboard with hot-reload (proxies API to localhost:8080)
	cd web/dashboard && npm install && npm run dev

local-stop: ## Stop all locally running processes (central, satellite, port-forward)
	@for name in central satellite pg-forward; do \
		if [ -f $(LOCAL_PID_DIR)/$$name.pid ]; then \
			pid=$$(cat $(LOCAL_PID_DIR)/$$name.pid); \
			if kill -0 $$pid 2>/dev/null; then \
				kill $$pid && echo "stopped $$name (pid $$pid)"; \
			else \
				echo "$$name already stopped"; \
			fi; \
			rm -f $(LOCAL_PID_DIR)/$$name.pid; \
		fi; \
	done
