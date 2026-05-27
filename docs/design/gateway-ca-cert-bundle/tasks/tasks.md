# Gateway CA Certificate Bundle — Implementation Plan

## Context

The MCP Gateway's custom TLS support (#659, #1008) embeds per-server CA PEM data inline in the config secret. When many servers share the same CA, this causes duplication, config bloat, and operational overhead. This feature adds a `caCertBundleRef` field to `MCPGatewayExtension` for a shared trust pool.

Design: `docs/design/gateway-ca-cert-bundle/gateway-ca-cert-bundle-design.md`

## Existing Code to Build On

### Per-server CA cert flow (already implemented)
1. **CRD field**: `MCPServerRegistrationSpec.CACertSecretRef` (`api/v1alpha1/types.go:88-93`)
2. **Type**: `CACertSecretReference` with `Name` and `Key` (default `ca.crt`) (`api/v1alpha1/types.go:165-177`)
3. **Controller**: reads secret via `DirectAPIReader`, validates label/key/size/PEM, writes inline to config (`mcpserverregistration_controller.go:447-479`)
4. **Config type**: `MCPServer.CACert string` (`internal/config/types.go:97`)
5. **Broker upstream**: `buildHTTPClient()` creates custom `*http.Client` with CA appended to system roots (`internal/broker/upstream/mcp.go:46-69`)
6. **Broker connect**: passes custom client via `transport.WithHTTPBasicClient()` (`internal/broker/upstream/mcp.go:140-146`)

### MCPGatewayExtension controller
- `reconcileActive()` in `mcpgatewayextension_controller.go` — orchestrates Deployment, Service, HTTPRoutes, EnvoyFilter
- `reconcileBrokerRouter()` — creates/updates the broker-router Deployment with volumes, env vars, and args
- Existing volume mount pattern: aggregated credentials secret is already mounted as a volume

## Implementation Order

Tasks are ordered by dependency.

### Task 1: CRD + config types

**Files:**
- `api/v1alpha1/mcpgatewayextension_types.go` — add `CACertBundleRef *CACertBundleReference` to `MCPGatewayExtensionSpec`
- `api/v1alpha1/mcpgatewayextension_types.go` — add `CACertBundleReference` struct with `Name` and `Key` (default `ca-bundle.crt`)
- `internal/config/types.go` — add `GatewayCACertBundle string` to `MCPServersConfig`
- Run `make generate-all` to regenerate deepcopy, CRDs, sync Helm

**Acceptance criteria:**
- [ ] CRD accepts `caCertBundleRef` with `name` (required) and `key` (optional, default `ca-bundle.crt`)
- [ ] `MCPServersConfig` has `GatewayCACertBundle` field
- [ ] `make generate-all && make lint` passes
- [ ] Existing tests pass without changes

**Verification:** `make generate-all && make lint && make test-unit`

---

### Task 2: MCPGatewayExtension controller — validation + volume mount

Depends on: Task 1.

**Files:**
- `internal/controller/mcpgatewayextension_controller.go` — add `reconcileCACertBundle()`:
  1. If `caCertBundleRef` is nil, skip
  2. Read the referenced Secret via `DirectAPIReader`
  3. Validate: exists, has `mcp.kuadrant.io/secret=true` label, key exists, size ≤ 256 KiB, valid PEM
  4. Set status condition on error
- `internal/controller/broker_router.go` — modify `reconcileBrokerRouter()`:
  1. If `caCertBundleRef` is set, add Secret volume + volume mount to Deployment
  2. Add `--ca-cert-bundle-path=/etc/mcp-gateway/ca/<key>` arg to broker container
  3. Add annotation hash of CA bundle content for rollout triggers
- `internal/controller/mcpgatewayextension_controller.go` — add Secret watch for labeled Secrets (already exists for MCPServerRegistration, extend to MCPGatewayExtension)
- `internal/controller/mcpgatewayextension_controller_test.go` — unit tests

**Volume details:**
- Volume name: `ca-cert-bundle`
- Mount path: `/etc/mcp-gateway/ca/`
- Read-only: true

**Acceptance criteria:**
- [ ] Valid CA bundle secret → volume mounted, arg added, status Ready
- [ ] Missing secret → status condition with error message
- [ ] Missing label → status condition with error message
- [ ] Invalid PEM → status condition with error message
- [ ] Size exceeds 256 KiB → status condition with error message
- [ ] Secret update triggers Deployment rollout via annotation hash
- [ ] No `caCertBundleRef` → no volume, no arg (backward compatible)

**Verification:** `make lint && make test-unit && make test-controller-integration`

---

### Task 3: Broker — load gateway CA bundle + build base trust pool

Depends on: Task 2.

**Files:**
- `cmd/mcp-broker-router/main.go` — add `--ca-cert-bundle-path` flag, pass to broker
- `internal/broker/broker.go` — add `CACertBundlePath string` field, load PEM from file at startup, build base `*x509.CertPool`
- `internal/broker/upstream/mcp.go` — modify `buildHTTPClient()`:
  1. Accept an optional base `*x509.CertPool` parameter (the gateway CA bundle pool)
  2. Clone the base pool (or system roots if nil)
  3. Append per-server `CACert` if set
  4. Return custom client with combined pool
- `internal/broker/upstream/mcp_test.go` — unit tests for combined pool behavior

**Acceptance criteria:**
- [ ] `--ca-cert-bundle-path` flag parsed and plumbed to broker
- [ ] Broker loads PEM from file at startup, logs cert count
- [ ] `buildHTTPClient()` uses base pool when available
- [ ] Per-server `CACert` appends to base pool (not replaces)
- [ ] No `--ca-cert-bundle-path` → behavior identical to current (system roots only)
- [ ] Invalid PEM file → broker logs error, falls back to system roots

**Verification:** `make test-unit`

---

### Task 4: Broker — file watch for CA bundle hot reload

Depends on: Task 3.

**Files:**
- `internal/broker/broker.go` — add file watcher for `CACertBundlePath`:
  1. Watch for file changes via `fsnotify` or polling (Kubernetes volume mount propagation)
  2. On change, rebuild base `*x509.CertPool`
  3. Re-register all servers to pick up new trust pool
- `internal/broker/broker_test.go` — unit tests for reload

**Acceptance criteria:**
- [ ] File change detected and pool rebuilt
- [ ] All servers re-registered with new pool
- [ ] Log message on reload: `reloaded gateway CA bundle certs=N`
- [ ] No crash on temporary file absence during Secret rotation
- [ ] No `--ca-cert-bundle-path` → no watcher started

**Verification:** `make test-unit`

---

### Task 5: MCPServerRegistration controller — stop embedding shared CAs in config

Depends on: Task 2. Independent of Tasks 3-4 (controller-side only).

**Files:**
- `internal/controller/mcpserverregistration_controller.go` — modify config building:
  1. If the server's `caCertSecretRef` points to the same Secret as the gateway's `caCertBundleRef`, skip embedding `CACert` in the config (it's already mounted as a volume)
  2. If different Secret, continue embedding as before
- `internal/controller/mcpserverregistration_controller_integration_test.go` — integration tests

**Note:** This is an optimization, not strictly required. Without it, the broker receives the CA both via volume mount and inline config — functionally correct but wasteful. This task prevents the duplication that motivates the feature.

**Acceptance criteria:**
- [ ] Server CA that matches gateway bundle → `CACert` omitted from config
- [ ] Server CA that differs from gateway bundle → `CACert` embedded as before
- [ ] No gateway bundle configured → all server CAs embedded as before

**Verification:** `make test-unit && make test-controller-integration`

---

### Task 6: Documentation and guide updates

Depends on: Tasks 1–4.

**Files:**
- `docs/guides/custom-ca-certificates.md` — add section for gateway-level CA bundle
- `docs/reference/mcpgatewayextension.md` — add `caCertBundleRef` field documentation
- `AGENTS.md` — update with `caCertBundleRef` information

**Acceptance criteria:**
- [ ] Guide covers: when to use gateway bundle vs per-server CA, step-by-step setup, rotation
- [ ] API reference updated with field, type, default, constraints
- [ ] AGENTS.md updated

---

### Task 7: E2E tests

Depends on: Tasks 1–4.

**Files:**
- `tests/e2e/ca_bundle_test.go` (new) — E2E test cases

**Acceptance criteria:**
- [ ] All e2e test cases from `e2e_test_cases.md` pass
- [ ] Tests clean up resources after each case

**Verification:** `make test-e2e`

---

## Verification (full)

```bash
make generate-all       # CRD regeneration
make lint               # Style checks
make test-unit          # All unit tests
make test-controller-integration  # Controller integration tests
make test-e2e           # E2E with Kind cluster
```
