# pg-swarm Docker Deployment

Local development and single-host deployment using Docker and Docker Compose.

## Files

| File | Description |
|------|-------------|
| `Dockerfile.central` | Multi-stage build for the central control plane |
| `Dockerfile.satellite` | Multi-stage build for the satellite agent |
| `docker-compose.yml` | Full stack: PostgreSQL + central + satellite |

## Dockerfiles

### Dockerfile.central

Two-stage build targeting a `scratch` final image (~10 MB):

- **Builder** (`golang:1.25-alpine`): downloads dependencies as a separate layer for cache reuse, then compiles a fully static binary (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`).
- **Runtime** (`scratch`): only the binary, CA certificates, and timezone data. No shell, no package manager.

Exposed ports:
- `9090` — gRPC (satellite registration and streaming)
- `8080` — REST API (management)

### Dockerfile.satellite

Two-stage build targeting `alpine:3.21`:

- **Builder** (`golang:1.25-alpine`): same static binary compilation as central. Also installs `postgresql-client` in the builder for `pg_basebackup` availability during replica bootstrap (Phase 3+).
- **Runtime** (`alpine:3.21`): Alpine base rather than scratch because the satellite requires `pg_isready` and `pg_basebackup` at runtime for health checks and replica initialisation.
- Runs as a non-root `pgswarm` user.
- `/var/lib/pg-swarm` is created with `0700` permissions for persisting `identity.json`.

## Quick Start (Docker Compose)

Bring up the full stack — PostgreSQL, central, and a local satellite — in one command:

```bash
# From the repository root
make docker-compose-up

# Or directly
docker compose -f deploy/docker/docker-compose.yml up --build -d
```

### What starts

| Service | Image | Ports |
|---------|-------|-------|
| `postgres` | `postgres:17-alpine` | internal only |
| `central` | built from `Dockerfile.central` | `9090`, `8080` |
| `satellite` | built from `Dockerfile.satellite` | none |

Start order is enforced: postgres readiness is checked with `pg_isready` before central starts. The satellite starts after central is running.

### Verify it's working

```bash
# Check all containers are up
docker compose -f deploy/docker/docker-compose.yml ps

# List satellites (should be empty initially)
curl http://localhost:8080/api/v1/satellites

# Follow satellite logs to watch registration
docker compose -f deploy/docker/docker-compose.yml logs -f satellite
```

The satellite will register and then poll for approval every 5 seconds. You should see:

```
registered with central, waiting for approval...
```

Approve it:

```bash
# Get the satellite ID from the list
SATELLITE_ID=$(curl -s http://localhost:8080/api/v1/satellites | jq -r '.[0].id')

# Approve it
curl -X POST http://localhost:8080/api/v1/satellites/${SATELLITE_ID}/approve
```

The satellite logs will then show:

```
approved by central
satellite agent started
connected to central stream
```

### Tear down

```bash
make docker-compose-down
# or
docker compose -f deploy/docker/docker-compose.yml down -v
```

The `-v` flag removes the named volumes (`postgres_data`, `satellite_data`). Omit it to retain data between restarts.

## Building Images Standalone

```bash
# From the repository root
make docker-build

# Override repository and tag
make docker-build DOCKER_REPO=myregistry.io/myorg IMAGE_TAG=v0.2.0
```

This runs:

```bash
docker build -f deploy/docker/Dockerfile.central  -t ghcr.io/pg-swarm/pg-swarm-central:latest  .
docker build -f deploy/docker/Dockerfile.satellite -t ghcr.io/pg-swarm/pg-swarm-satellite:latest .
```

Note: the build context is the **repository root** (not this directory) so the full source tree is available.

## Pushing Images

```bash
make docker-push

# With custom registry
make docker-push DOCKER_REPO=myregistry.io/myorg IMAGE_TAG=v0.2.0
```

## Environment Variables

### Central

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | — | PostgreSQL connection string (required) |
| `GRPC_ADDR` | `:9090` | gRPC listen address |
| `HTTP_ADDR` | `:8080` | REST API listen address |

### Satellite

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `CENTRAL_ADDR` | `localhost:9090` | No | Central gRPC address |
| `HOSTNAME` | OS hostname | No | Satellite identifier |
| `K8S_CLUSTER_NAME` | — | **Yes** | Kubernetes cluster name |
| `REGION` | `""` | No | Geographic region label |
| `DATA_DIR` | `/var/lib/pg-swarm` | No | Directory for persisting `identity.json` |

## Notes

- The satellite persists its `identity.json` to `DATA_DIR`. Mount a volume here to survive container restarts without re-registration.
- In `docker-compose.yml` the satellite connects to central via the Docker network hostname `central`. For standalone containers on separate hosts, set `CENTRAL_ADDR` to the central's external address.
