# Gateway-Level CA Certificate Bundle

## Problem

With custom TLS support ([#659](https://github.com/Kuadrant/mcp-gateway/issues/659), [#1008](https://github.com/Kuadrant/mcp-gateway/pull/1008)), each `MCPServerRegistration` can reference its own CA certificate via `caCertSecretRef`. When many upstream servers share the same root or intermediate CA, this results in:

- **Duplication** — the same CA PEM repeated N times across N registrations. Common in environments using OpenShift service-serving CA, a shared cert-manager issuer, or a corporate root CA.
- **Config secret bloat** — each CA cert (up to 64 KiB per the `maxCACertSize` limit in the controller) is embedded per-server in the config YAML passed to the broker. The config is stored in a Kubernetes Secret (`mcp-gateway-config`), which is subject to the 1 MiB Secret limit. With 15 servers sharing the same 64 KiB CA bundle, the config consumes ~960 KiB on CA certs alone, leaving no room for actual configuration.
- **Operational overhead** — CA rotation requires updating N Secrets and waiting for N re-reconciliations instead of one. Each `MCPServerRegistration` re-reconciles independently, creating a window where some servers use the old CA and some use the new one.

## Summary

Add a `caCertBundleRef` field to `MCPGatewayExtension` that references a shared CA certificate bundle Secret. The broker loads this bundle at startup as a base trust pool. Per-server `caCertSecretRef` on `MCPServerRegistration` appends additional CAs for backends with unique CAs. This mirrors how Kubernetes itself works — cluster-wide CA bundle plus per-resource overrides.

## Goals

- Eliminate CA PEM duplication in the config secret when multiple servers share the same CA
- Reduce operational burden for CA rotation to a single Secret update
- Maintain backward compatibility — existing `caCertSecretRef` on `MCPServerRegistration` continues to work identically
- Keep the CA PEM data out of the per-server config entries in the config secret

## Non-Goals

- Replace `caCertSecretRef` on `MCPServerRegistration` (still needed for server-specific CAs)
- Modify how Envoy/Gateway handles TLS (this only affects broker-to-upstream connections)
- Implement mutual TLS (mTLS) between broker and upstream servers
- Support non-PEM certificate formats

## Job Stories

### When I have many servers behind the same private CA

When a platform engineer has 10+ upstream MCP servers behind the same OpenShift service-serving CA, they want to configure the CA once at the gateway level so that they don't need to create and maintain separate CA Secrets for each `MCPServerRegistration`.

### When I need to rotate a shared CA certificate

When a platform engineer needs to rotate the CA certificate used by all upstream servers (e.g. cert-manager issuer renewal), they want to update a single Secret and have all servers pick up the new CA within one reconciliation cycle, instead of updating N Secrets and waiting for N independent reconciliations.

### When I have servers with unique CAs alongside a shared CA

When a platform engineer has most servers behind a shared corporate CA but one or two servers using their own private CA, they want to set the shared CA at the gateway level and the unique CAs per-server, so that both types work without duplication.

### When I want to avoid hitting the Kubernetes Secret size limit

When a platform engineer registers many servers that each embed CA PEM data in the config secret, they want the shared CA to be loaded once by the broker (not embedded N times in the config), so that the config secret stays well under the 1 MiB Kubernetes limit.

## Design

### API Changes

#### MCPGatewayExtension

New optional field `caCertBundleRef` on `MCPGatewayExtensionSpec`:

```yaml
apiVersion: mcp.kuadrant.io/v1alpha1
kind: MCPGatewayExtension
metadata:
  name: mcp-gateway
  namespace: mcp-system
spec:
  targetRef:
    name: mcp-gateway
    sectionName: mcp
  caCertBundleRef:
    name: shared-ca-bundle
    key: ca-bundle.crt    # optional, defaults to "ca-bundle.crt"
```

```go
// MCPGatewayExtensionSpec gains:
type MCPGatewayExtensionSpec struct {
    // ... existing fields ...

    // caCertBundleRef references a Secret containing a PEM-encoded CA certificate
    // bundle used as the base trust pool for all upstream MCP server connections.
    // Per-server caCertSecretRef on MCPServerRegistration appends to this pool.
    // The Secret must have the label mcp.kuadrant.io/secret=true.
    // +optional
    CACertBundleRef *CACertBundleReference `json:"caCertBundleRef,omitempty"`
}

// CACertBundleReference identifies a Secret containing a PEM-encoded CA bundle.
type CACertBundleReference struct {
    // name is the name of the Secret resource.
    // +required
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name,omitempty"`

    // key is the key within the Secret that contains the CA bundle PEM data.
    // If not specified, defaults to "ca-bundle.crt".
    // +optional
    // +default="ca-bundle.crt"
    Key string `json:"key,omitempty"`
}
```

The default key is `ca-bundle.crt` (not `ca.crt`) to distinguish from per-server CA secrets and align with common Kubernetes conventions (e.g. OpenShift's `ca-bundle.crt` in ConfigMaps).

#### Config Type Changes

`MCPServersConfig` in `internal/config/types.go` gains a field for the gateway-level CA bundle:

```go
type MCPServersConfig struct {
    // ... existing fields ...

    // GatewayCACertBundle is the PEM-encoded CA bundle from MCPGatewayExtension.
    // Loaded once by the broker, not embedded per-server.
    GatewayCACertBundle string
}
```

This field is not serialized into the per-server config YAML — it is passed to the broker via a separate volume mount of the CA Secret, avoiding config secret bloat.

### Architecture

#### How the CA Bundle Reaches the Broker

```
MCPGatewayExtension                Broker Pod
┌──────────────────┐         ┌────────────────────────┐
│ caCertBundleRef: │         │                        │
│   name: shared-  │────────▶│  Volume Mount:         │
│         ca-bundle│         │  /etc/mcp-gateway/ca/  │
│   key: ca-bundle │         │    ca-bundle.crt       │
│         .crt     │         │                        │
└──────────────────┘         │  Broker reads at       │
                             │  startup + watches     │
                             │  for changes           │
                             └────────────────────────┘
```

The MCPGatewayExtension controller:
1. Validates the referenced CA Secret exists, has the required label, contains valid PEM, and is within size limits
2. Adds the Secret as a volume mount on the broker-router Deployment
3. Passes the mount path via a command-line flag `--ca-cert-bundle-path=/etc/mcp-gateway/ca/ca-bundle.crt`

The broker:
1. Reads the CA bundle PEM from the file at startup
2. Builds a base `*x509.CertPool` from system roots + gateway CA bundle
3. For each upstream server, clones this base pool and appends any per-server CA from the config (if `CACert` is set on the `MCPServer`)
4. Watches the file for changes (Kubernetes volume mount propagation) and rebuilds the pool on update

#### Trust Pool Construction

```
System Root CAs (Go default)
         ├── + Gateway CA Bundle (from caCertBundleRef)
         │        = Base Trust Pool (shared by all servers)
         │
         ├── Server A: uses base pool only (no caCertSecretRef)
         ├── Server B: uses base pool only (no caCertSecretRef)
         └── Server C: base pool + per-server CA (caCertSecretRef set)
                       = Server-specific pool
```

When `caCertBundleRef` is not set, behavior is identical to today — each server uses system roots + its own `caCertSecretRef` if set.

### Interaction with Per-Server caCertSecretRef

| Gateway `caCertBundleRef` | Server `caCertSecretRef` | Effective Trust Pool |
|---------------------------|--------------------------|----------------------|
| Not set                   | Not set                  | System roots only |
| Not set                   | Set                      | System roots + server CA (current behavior) |
| Set                       | Not set                  | System roots + gateway CA bundle |
| Set                       | Set                      | System roots + gateway CA bundle + server CA |

Per-server `caCertSecretRef` always **appends** — it never replaces the gateway bundle. This is additive-only, following the Kubernetes pattern where more-specific configuration adds to less-specific configuration rather than overriding it.

### Validation

The MCPGatewayExtension controller validates `caCertBundleRef` during reconciliation:

| Check | Error |
|-------|-------|
| Secret exists | `CA bundle secret <name> not found` |
| Secret has `mcp.kuadrant.io/secret=true` label | `CA bundle secret <name> missing required label` |
| Key exists in Secret | `CA bundle secret <name> missing key <key>` |
| PEM data is valid | `CA bundle in secret <name> is invalid: <reason>` |
| Size ≤ 256 KiB | `CA bundle data exceeds maximum size` |

The size limit is 256 KiB (vs 64 KiB for per-server CAs) because a shared bundle typically contains multiple CA certificates (root + intermediates, or multiple issuer CAs).

### Volume Mount Strategy

The CA bundle Secret is mounted as a read-only volume, not embedded in the config secret. This:

1. **Avoids config bloat** — the CA PEM is not duplicated per-server in the config YAML
2. **Enables hot reload** — Kubernetes propagates Secret updates to volume mounts (60-120s kubelet sync), and the broker watches the file for changes
3. **Follows the existing pattern** — the aggregated credentials secret is already mounted as a volume on the broker-router Deployment

Mount details:
- Volume name: `ca-cert-bundle`
- Mount path: `/etc/mcp-gateway/ca/`
- File: `ca-bundle.crt` (or the configured key)
- Read-only: true

### CA Bundle Rotation

When the CA bundle Secret is updated:

1. The MCPGatewayExtension controller detects the Secret update (via watch on labeled Secrets)
2. The controller re-validates the PEM and updates the Deployment annotation hash to trigger a rollout (if the content changed)
3. Kubernetes propagates the new Secret data to the volume mount (60-120s)
4. The broker detects the file change, rebuilds the base trust pool, and re-registers all servers that use it

End-to-end propagation: ~60-120 seconds (dominated by kubelet volume sync).

**Alternative: file watch without rollout.** If rollout restarts are undesirable (they briefly interrupt all connections), the broker can use `fsnotify` to watch the mounted file and rebuild the cert pool in-place. This avoids pod restarts but relies on volume mount propagation being timely. Both strategies should be evaluated during implementation.

## Security Considerations

- **Same label requirement** — the CA bundle Secret must have `mcp.kuadrant.io/secret=true`, consistent with per-server CA Secrets and credential Secrets
- **No new trust** — the gateway CA bundle extends the system root pool, it does not replace it. Servers that work today with publicly-trusted CAs continue to work without changes
- **Additive-only** — per-server CAs always append to the gateway bundle, never override. An operator cannot accidentally remove trust for a server by setting the gateway bundle
- **Size limit** — 256 KiB prevents accidental inclusion of large files. This is generous enough for typical CA chains (~20-30 CAs at ~2 KiB each)

## Future Considerations

### ConfigMap Support

The current design uses a Secret for the CA bundle. A future enhancement could support ConfigMaps, which are more natural for non-sensitive CA certificates and align with how OpenShift distributes the service-serving CA (`openshift-service-ca.crt` ConfigMap). This would require a union-type field or separate `caCertBundleConfigMapRef`.

### Certificate Expiry Monitoring

The broker could parse the CA certificates and emit metrics or log warnings when certificates are near expiry. This would help operators detect upcoming CA rotations before they cause outages.

## Execution

See:
- [tasks/tasks.md](tasks/tasks.md) for the implementation plan
- [tasks/e2e_test_cases.md](tasks/e2e_test_cases.md) for E2E test cases
- [tasks/documentation.md](tasks/documentation.md) for documentation outline
