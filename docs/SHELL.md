# Web-Based Pod Shell from Cluster Details Page

## Context

Add a browser-based terminal into any pod's postgres container from the cluster details page. The user clicks "Shell" on an instance row, a terminal modal opens, and they can run commands interactively. The exec path is: **Browser (WebSocket) → Central → Satellite (gRPC) → K8s pods/exec (SPDY) → Container**.

## Architecture

The satellite has no inbound listener — central can't reach it directly. All communication goes through the existing bidirectional gRPC stream. We add new protobuf message types for exec I/O and multiplex them over this stream using a `session_id` to route between concurrent sessions.

```
Browser ──WS──▶ Central ──gRPC──▶ Satellite ──SPDY──▶ K8s API ──▶ Container
       ◀──WS── Central ◀──gRPC── Satellite ◀──────── K8s API ◀── Container
```

## Files to Create

| File | Purpose |
|---|---|
| `api/proto/v1/config.proto` | Add ExecOpen/Input/Resize/Close/Output/Exit messages |
| `internal/satellite/exec/handler.go` | Session manager: K8s SPDY exec with stdin pipe, resize, cleanup |
| `internal/central/server/exec.go` | Exec session registry + WebSocket handler at `/api/v1/exec` |
| `web/dashboard/src/components/PodTerminal.jsx` | xterm.js terminal component wired to WebSocket |

## Files to Modify

| File | Change |
|---|---|
| `internal/satellite/agent/agent.go` | Store `rest.Config`, wire exec callbacks |
| `internal/satellite/agent/registration.go` | Return `rest.Config` from `buildK8sClient()` |
| `internal/satellite/stream/connector.go` | Add OnExecOpen/Input/Resize/Close callbacks + dispatch |
| `internal/central/server/grpc.go` | Forward ExecOutput/ExecExit to browser WebSocket |
| `internal/central/server/rest.go` | Register `/api/v1/exec` WebSocket route |
| `web/dashboard/src/pages/ClusterDetail.jsx` | Add Shell button per instance + terminal modal |
| `web/dashboard/src/api.js` | Add `execWsUrl()` helper |
| `web/dashboard/package.json` | Add `@xterm/xterm`, `@xterm/addon-fit`, `@xterm/addon-web-links` |

## Implementation Order

### Step 1: Protobuf messages

Add to `api/proto/v1/config.proto`:

```protobuf
message ExecOpen {
  string session_id = 1;
  string cluster_name = 2;
  string namespace = 3;
  string pod_name = 4;
  string container = 5;       // default: "postgres"
  repeated string command = 6; // default: ["/bin/bash"]
  uint32 rows = 7;
  uint32 cols = 8;
}
message ExecInput  { string session_id = 1; bytes data = 2; }
message ExecResize { string session_id = 1; uint32 rows = 2; uint32 cols = 3; }
message ExecClose  { string session_id = 1; }
message ExecOutput { string session_id = 1; bytes data = 2; }
message ExecExit   { string session_id = 1; int32 exit_code = 2; string error_message = 3; }
```

Add to `CentralMessage.payload` oneof: `ExecOpen`, `ExecInput`, `ExecResize`, `ExecClose`.
Add to `SatelliteMessage.payload` oneof: `ExecOutput`, `ExecExit`.

Run `make proto`.

### Step 2: Satellite exec handler (`internal/satellite/exec/handler.go`)

- `SessionManager` struct with `sync.Map` of `session_id → *ExecSession`
- `Open()`: creates SPDY executor via `remotecommand.NewSPDYExecutor` (pattern from `failover/monitor.go:278`), starts `StreamWithContext` in goroutine with `Tty: true`, stdin via `io.Pipe`, `TerminalSizeQueue` via channel
- `WriteStdin()`, `Resize()`, `Close()` methods
- 30-minute session timeout, auto-cleanup on gRPC disconnect

### Step 3: Satellite wiring

- `registration.go`: change `buildK8sClient()` to also return `*rest.Config`, store on Agent
- `connector.go`: add `OnExecOpen/Input/Resize/Close` callback fields + dispatch cases
- `agent.go`: create `exec.SessionManager`, wire callbacks to send ExecOutput/ExecExit via `connector.SendMessage()`

### Step 4: Central exec proxy (`internal/central/server/exec.go`)

- `ExecSessionRegistry`: maps `session_id → *ExecSessionProxy` (browser WS + satellite ID)
- WebSocket handler at `/api/v1/exec?satellite_id=X&cluster_name=Y&namespace=Z&pod_name=W&cols=80&rows=24`
- Browser→Central: JSON `{type: "input"|"resize"|"close", data?, cols?, rows?}` → translated to gRPC ExecInput/Resize/Close sent to satellite
- Central→Browser: ExecOutput/ExecExit from gRPC → JSON `{type: "output"|"exit", data?, exit_code?}` to WS
- Terminal output bytes base64-encoded in JSON

### Step 5: Central gRPC dispatch (`grpc.go`)

Add cases in `Connect()` read loop for `ExecOutput` and `ExecExit` → look up session in registry → forward to browser WebSocket.

### Step 6: Frontend

Install: `@xterm/xterm`, `@xterm/addon-fit`, `@xterm/addon-web-links`

**PodTerminal.jsx**: xterm.js instance, opens WS to `/api/v1/exec`, wires `terminal.onData → ws input`, `ws output → terminal.write`, `fitAddon resize → ws resize`. Dark theme matching SatelliteLogs.jsx (#0d1117 bg).

**ClusterDetail.jsx**: Add Terminal icon button per instance row. On click, set `shellPod` state → render modal overlay with `<PodTerminal>`. Close button + Escape key dismiss.

### Step 7: Cleanup and timeouts

- Satellite: 30-min context deadline per session, cleanup on gRPC reconnect
- Central: periodic scan (60s) for stale sessions, cleanup on WS disconnect
- Browser: show "Connection lost" on WS close, "Reconnect" button, cleanup on unmount

## Verification

1. `make proto` — regenerates cleanly
2. `go build ./...` — compiles
3. `make test` — passes
4. `make dashboard` — builds frontend
5. Manual: deploy to minikube, open cluster details, click Shell on a pod, run `psql -U postgres`, verify interactive output
