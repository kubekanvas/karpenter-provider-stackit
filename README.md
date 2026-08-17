# karpenter-provider-stackit

A [Karpenter](https://karpenter.sh) cloud provider for [STACKIT](https://www.stackit.de), built
against `sigs.k8s.io/karpenter` v1.14.0 and the STACKIT IaaS v2 API.

It provisions STACKIT IaaS servers for pending pods, consolidates underutilized nodes, and cleans up
servers whose NodeClaim has gone away.


## Why the details below matter

Most of what makes a Karpenter provider work — or silently fail — is agreement with the cluster's
cloud-controller-manager about how a node is identified and labelled. This section documents what
this provider does and why, so that a future STACKIT change can be checked against it.

Everything here was verified on **2026-08-15** against
[`cloud-provider-stackit`](https://github.com/stackitcloud/cloud-provider-stackit) at commit
`83f9aae` and `stackit-sdk-go/services/iaas` **v1.14.0**.

### providerID

This provider writes:

```
stackit://<server-uuid>
```

That is byte-for-byte what the CCM produces in `Instances.makeInstanceID`
(`pkg/ccm/instances.go`), which is literally `fmt.Sprintf("%s://%s", "stackit", server.GetId())` —
two slashes, no region segment. Karpenter matches NodeClaims to Nodes on this exact string, so any
other shape means NodeClaims never bind and every launch is garbage collected.

The CCM's *parser* is more permissive than its writer: it also accepts a bare `<id>` and a legacy
`openstack://<region>/<id>` form, and it declares an `OS_CCM_REGIONAL` environment variable.
**None of those are written under any supported configuration** — `NewInstance` hardcodes
`regionProviderID: false` and nothing reads it back. Emitting the legacy form would still *parse*
but would not match, so this provider emits only the canonical form.

`TestProviderIDMatchesCCMFormat` pins this against the CCM's own regexp.

### Zone and region

STACKIT has a real region/availability-zone split. This is the main structural difference from the
sibling UpCloud provider, where both topology labels carry the same value.

| Label | Set by the CCM from | Set by this provider from |
|---|---|---|
| `topology.kubernetes.io/zone` | the server's own `availabilityZone`, passed through `labels.Sanitize` | the server's `availabilityZone`, through the same sanitizer |
| `topology.kubernetes.io/region` | the CCM's **own configuration** (`global.region`) — never read off the server | the controller's `--region` flag |

Two consequences:

1. **`--region` must equal the CCM's configured region.** Nothing can detect a mismatch at startup;
   it surfaces as NodeClaims that never register. There is no per-NodeClass region field, precisely
   so a NodeClass cannot contradict the CCM.
2. **Zones are sanitized on this side too.** `utils.SanitizeZone` reproduces `labels.Sanitize` from
   `cloud-provider-stackit/pkg/labels/labels.go`: non-alphanumerics other than `-`, `_` and `.`
   collapse to `-`, leading/trailing `-_.` are trimmed, and the result is capped at 63 characters.
   Real STACKIT zone ids (`eu01-1`) pass through unchanged, but applying the transform anyway is
   what guarantees an offering's zone requirement can match the node label.

### Boot volumes and the delete flow

STACKIT creates the boot volume as a separate resource and then boots the server from it. Two facts
combine into the most expensive mistake available here:

- `DeleteServer` is documented as **"Delete a server. Volumes won't be deleted."**
- `BootVolume.deleteOnTermination` **defaults to `false`** server-side.

Left alone, that leaks a billable root disk for every node Karpenter ever terminates. This provider
therefore defaults `spec.bootVolume.deleteOnTermination` to `true` and sets it explicitly on every
create. You can set it to `false`, but nothing in this controller will then track or reclaim those
volumes.

`Delete` uses Karpenter's retry contract rather than blocking:

1. First call: `GET` the server, issue `DeleteServer`, return a non-nil error meaning "in progress".
2. Subsequent calls: observe `DELETING` and return "still in progress" without re-issuing.
3. The call that finally sees a 404 returns `NodeClaimNotFoundError`, and Karpenter stops.

A 409 is treated as "busy, retry" rather than as a failure. STACKIT accepts a delete on a running
server, so there is no stop-then-delete dance.

### Instance creation time

`Server.createdAt` is returned by the API on both `GetServer` and `ListServers`, so garbage
collection reads the real timestamp and this provider records no launch-time label of its own
(the UpCloud provider has to, because UpCloud reports no creation time).

If `createdAt` is ever absent, the garbage collector **skips** the server and logs it rather than
treating it as infinitely old. Deleting a live server because a field failed to parse is not
recoverable.

### Listing and labels

`ListServers` supports `details=true` and a `labelSelector`, so labels come back with the list and
the managed-by filter is applied server-side. There is no per-server detail call to fan out or
bound — again unlike UpCloud, whose list response omits labels.

The controller stamps these keys on every server it launches:

| Key | Value |
|---|---|
| `karpenter.sh/managed-by` | the cluster name; this is the List filter |
| `karpenter.sh/nodepool` | owning NodePool |
| `karpenter.sh/nodeclaim` | owning NodeClaim |
| `karpenter.k8s.stackit/nodeclass` | owning StackitNodeClass |

They are reserved: `spec.labels` may not set them. A server missing `karpenter.sh/managed-by` is
invisible to this controller and will never be garbage collected, so two clusters sharing a STACKIT
project must not share a `clusterName`.

Label keys may contain `/` (the machine-controller-manager provider uses `kubernetes.io/machine`).
Validation rejects whitespace, non-printable-ASCII, and — specifically — `,` and `=`, because those
delimit the `key=value,key=value` selector string and a label containing one would make its server
undiscoverable.

### Capacity type

Hardcoded to `on-demand`, in both instance type requirements and offerings. The STACKIT IaaS API
exposes no spot, preemptible or reserved capacity concept at all.

### Pricing

**STACKIT publishes no price list API.** The IaaS `MachineType` model carries only `name`, `vcpus`,
`ram`, `disk`, `description` and `extraSpecs`; the `cost` service (`v3api`) reports spend already
incurred per project and cannot price a machine type that is not already running.

Karpenter does not actually need absolute prices — it needs prices whose *ratios* are right, since
they decide which instance type it picks and whether consolidating two nodes into one saves money.
Emitting `0.0` would make every node look free and consolidation nonsensical.

So prices are **modeled** from each machine type's resources:

```
price/hour = vCPUs × 0.0246 € + RAM_GiB × 0.0033 € + disk_GiB × 0.00007 €
```

The coefficients are anchored to STACKIT's published pay-as-you-go list pricing for `g1` flavors
as of 2026-08-15, decomposed into per-vCPU and per-GiB components. **They are a modeling choice,
not a quote** — your invoice depends on your contract, which this controller cannot read.

If you want the exported cost metrics to be absolute, override per machine type:

```yaml
settings:
  pricingOverrides: "g1.2=0.0716,g1.4=0.1432"
```

The model is guaranteed to return a strictly positive price for every machine type.

### Known gap: GPU machine types

GPU machine types are advertised on their CPU and memory only — no `nvidia.com/gpu` extended
resource. STACKIT reports GPU details, if at all, inside `MachineType.extraSpecs`, whose keys the
IaaS API does not document and which neither `cloud-provider-stackit` nor
`machine-controller-manager-provider-stackit` reads. Guessing a key would silently mis-advertise
capacity. This is straightforward to add once the real `extraSpecs` shape is confirmed against an
account with GPU flavors enabled.

## Installation

Requires a cluster already running
[`cloud-provider-stackit`](https://github.com/stackitcloud/cloud-provider-stackit) with
`--cloud-provider=external` on the kubelets.

```bash
helm install karpenter-crd oci://ghcr.io/kubekanvas/karpenter-provider-stackit/charts/karpenter-crd \
  --namespace karpenter --create-namespace
```

```bash
helm install karpenter oci://ghcr.io/kubekanvas/karpenter-provider-stackit/charts/karpenter \
  --namespace karpenter \
  --set settings.clusterName=my-cluster \
  --set settings.projectID=<stackit-project-id> \
  --set settings.region=eu01 \
  --set credentials.existingSecret=stackit-credentials
```

The CRD chart is installed separately so that a `helm uninstall` of the controller cannot take the
CRDs — and with them every NodePool and NodeClaim — down with it.

### Credentials

The Secret must contain one of the following, whichever the STACKIT SDK should discover:

- `STACKIT_SERVICE_ACCOUNT_KEY` — preferred; the SDK refreshes it automatically
- `STACKIT_SERVICE_ACCOUNT_KEY_PATH`
- `STACKIT_SERVICE_ACCOUNT_TOKEN` — expires, and nothing renews it

```bash
kubectl create secret generic stackit-credentials \
  --namespace karpenter \
  --from-file=STACKIT_SERVICE_ACCOUNT_KEY=./sa-key.json
```

The service account needs permission to create, read, update and delete servers, and to read
machine types, availability zones and volumes in the project.

The chart can also create the Secret from `credentials.serviceAccountKey`, but that puts the key in
your values file and in the Helm release history; prefer `credentials.existingSecret`.

## Usage

See [`examples/v1alpha1`](examples/v1alpha1). The one field that catches everyone is
`spec.userData`: without a working bootstrap the server boots, never registers, and is garbage
collected once the registration TTL expires. The node must join with `--cloud-provider=external` so
that the CCM is what sets `spec.providerID`.

```bash
kubectl apply -f examples/v1alpha1/nodeclass.yaml
kubectl apply -f examples/v1alpha1/nodepool.yaml
kubectl get stackitnodeclass default -o wide
```

A NodeClass reports `Ready` once its availability zones resolve against the configured region and
its labels and networking validate.

### Well-known labels

Beyond the upstream ones, NodePools and pods can select on:

| Label | Example |
|---|---|
| `karpenter.k8s.stackit/instance-cpu` | `4` |
| `karpenter.k8s.stackit/instance-memory` | `16384` (MiB) |
| `karpenter.k8s.stackit/instance-family` | `g1` |
| `karpenter.k8s.stackit/instance-disk` | `0` (GiB, the machine type's own disk) |

Note that `instance-disk` is the machine type's bundled disk, which is `0` for most STACKIT types.
A node's **ephemeral storage** comes from `spec.bootVolume.size` instead, since STACKIT boots from a
separately created volume.

## Development

```bash
make build          # compile
make test           # unit tests with -race
make lint           # golangci-lint
make generate       # deepcopy + CRDs, and vendor the karpenter core CRDs
make verify         # regenerate everything and fail if anything changed
```

The Karpenter core CRDs (`NodePool`, `NodeClaim`, `NodeOverlay`) are vendored into `pkg/apis/crds`
and shipped in the CRD chart, so the controller installs exactly the versions it was built against.
The image and both charts are released from a single tag for the same reason: drift between the
CRDs embedded in the binary and those in the chart is a broken install.

Tool dependencies (`controller-gen`, `golangci-lint`, `ko`) are pinned in `go.tools.mod`.

## License

Apache 2.0. See [LICENSE](LICENSE).
