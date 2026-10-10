# Deploying to Kubernetes

The chart in `fired/` deploys:

- **fired**: one DaemonSet per node group, one pod per KVM node (bare-metal
  workstations joined to the cluster). Pods are privileged (Firecracker and
  the jailer need it; untrusted code runs inside the VMs), use the node's
  disks through `hostPath`, and mount the hot tier as a tmpfs on the node.
- **fired** Service: the single API endpoint. Any host answers for the
  whole cluster (see [docs/cluster.md](../docs/cluster.md)).
- **fired-db**: the Postgres fired keeps its state in (or use an external
  one).
- **fired-artifacts**: one replica on a volume, calling fired through the
  Service.
- **NetworkPolicies**: fired pods reach only their database, each other,
  DNS and the internet; only this namespace and the ones you list reach
  them.

## Nodes

Each KVM workstation joins the cluster as a node, labeled for its group and
tainted so nothing else runs there:

```bash
kubectl label node ws-1 fired=true fired-pool=chat
kubectl taint node ws-1 fired=true:NoSchedule
```

The node needs `/dev/kvm` and cgroup v2. fired's data lives on the node:
the hot tier (a tmpfs at `/srv/fired/hot`, created by the pod), and
`/var/lib/fired/{cold,templates}`. A node reboot loses the hot tier (running
and suspended sandboxes; archived ones survive): drain nodes deliberately,
and don't let an autoscaler remove them.

## Settings

The chart's defaults are in `fired/values.yaml`. Put your settings in
`values.yaml` here (gitignored), only what differs:

```yaml
token: "<openssl rand -hex 32>"      # fired's API token (your backend uses it too)
postgres:
  password: "<openssl rand -hex 16>"
artifacts:
  token: "<openssl rand -hex 32>"
  broker: {url: http://backend.app.svc/artifacts/credentials, token: "..."}
fired:
  groups:
    - {name: chat, nodeSelector: {fired-pool: chat}, pools: [chat]}
    - {name: runners, nodeSelector: {fired-pool: artifacts}, pools: [artifacts]}
  config:
    cluster: {default_pool: chat}
networkPolicy:
  allowFromNamespaces: [app]          # where your backend runs
```

Per-user limits aren't in values: they're tiers your backend defines through
the APIs (see [docs/cluster.md](../docs/cluster.md#per-user-limits)).

## Deploying

**CD:** every merge to `main` is tested, built and deployed by
`.github/workflows/ci.yml` (job `deploy`). It uses the GitHub environment
`cluster`:

| | |
|---|---|
| `vars.REGISTRY` | image repository, e.g. `registry.example.com/team` (the image is `$REGISTRY/fired:<commit>`) |
| `vars.NAMESPACE` | Kubernetes namespace (default `fired`) |
| `secrets.REGISTRY_USERNAME`, `secrets.REGISTRY_PASSWORD` | registry login |
| `secrets.KUBECONFIG` | base64 of a kubeconfig for the cluster (`base64 -w0 kubeconfig`) |
| `secrets.VALUES_YAML` | the contents of `values.yaml` |

**By hand,** with docker, helm and skaffold:

```bash
REGISTRY=registry.example.com/team ./deploy.sh
```

It uses `kubeconfig` here if present, else your current context.

**Upgrades** roll one node at a time. On SIGTERM each fired pod snapshots
its running sandboxes to the node's hot tier and the new pod resumes them
on demand (processes intact). The schema is migrated by the first upgraded
pod, under a lock; see [docs/cluster.md](../docs/cluster.md) for how schema
changes stay compatible with older pods during the rollout.
