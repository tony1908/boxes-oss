# Autoscaling nodes (VPS-agnostic)

Goal: extra VPS nodes are created when boxes need room and destroyed when they're idle, on any provider. Boxes are portable because snapshots live in shared S3 storage and any node can resume any box.

## Principles
- **cloud-init is the common contract.** A new node bootstraps itself with `node-install.sh` from its user_data: Docker, Sysbox, host sysctl, swap, Traefik, agent, storage config and registry login. It then joins with its own one-time join token.
- **Provider = create / get / delete / list-by-tag.** Nothing else is provider-specific. Built-in providers: `digitalocean` and `hetzner`, plus `script` (user-supplied executables) for everything else.
- **"Off" means destroyed.** Most providers bill powered-off VMs, and nodes hold no state (snapshots live in S3).
- Autoscaling never touches nodes that aren't in a pool: manual nodes and the control-plane node.

## Components
1. `internal/provider`: `Provider` interface `Create(ctx, NodeSpec{Name, Region, Size, Image, UserData, Tags, SSHKeys}) (Instance, error)`, `Get`, `Delete`, `List(ctx, tag)`; `Instance{ID, Name, PublicIPv4, Status}`. DigitalOcean (API v2, droplets) and Hetzner Cloud (servers) use plain `net/http`, no SDKs. `script`: exec `create` (stdin: JSON NodeSpec; stdout: JSON Instance), `delete <id>`, `list <tag>`, with a timeout, and it never goes through a shell.
2. **Pools** (owner-only API + web UI under Nodes): `{id, name, provider, credentials (encrypted), region, size, image, sshKeys, minNodes, maxNodes, idleMinutes (default 10), bootTimeoutMinutes (default 15)}`, with CRUD at `/api/v1/pools`. Nodes get `poolId` and `providerInstanceId`.
3. **Scale up:** when the scheduler finds no capacity, pick a pool with nodes < max (prefer the pool with most free headroom). Create a node record (status `provisioning`) plus a join token, then call `provider.Create` with cloud-init user_data that runs node-install with env: server URL, join token, host base (`<ip>.sslip.io` is decided by the node itself at install time), storage, registry. Boxes waiting for room stay `provisioning` with `waitingFor: node`, and several pending boxes share one new node when it fits. When the node connects, place the boxes. On boot timeout, delete the instance and mark the boxes `error` (no_capacity).
4. **Scale down:** every minute, a pool node with zero non-stopped boxes for `idleMinutes`, above `minNodes`, is drained and then deleted (`provider.Delete`, node record removed). Keep `minNodes` warm. Scale up to `minNodes` on startup.
5. **Reconcile:** on startup and every 10 minutes, `provider.List(tag=boxes-pool-<id>)`. Instances with no node record older than 30 minutes are deleted. Node records whose instance vanished are marked lost and their boxes `unreachable` (they resume elsewhere from S3).
6. **Built-in registry:** install.sh runs `registry:2` on 127.0.0.1:5000 of the control plane. Traefik routes `https://<domain>/v2/` to it with basic auth (generated credentials in `/etc/boxes/registry.env`). `boxes-server image push [--box IMAGE] [--tools IMAGE]` tags and pushes local images as `<domain>/boxes/box:<tag>` and `<domain>/boxes/tools:<tag>`. The server's `--image` / tools image point there. Nodes `docker login` with credentials passed through node-install env. Manual `POST /nodes` installs use it too.
7. **Prebuilt node images (optional, per provider):** `POST /api/v1/pools/{id}/bake` creates an instance from the base OS with a user_data install plus image pre-pull, waits for the agent to report ready, powers off, snapshots (DO: droplet snapshot action; Hetzner: `create_image`), deletes the instance and stores the image id in the pool. The `script` provider takes a user-supplied image.
8. **Security:** provider tokens are encrypted with the master key and never returned by the API. The UI recommends scoped tokens (DO custom scopes: droplet create/read/delete, image read). user_data carries per-node secrets: a one-time join token, storage creds and registry creds. Document that the provider can read user_data.

## Verification
Unit tests use a fake provider plus httptest fakes of the DO and Hetzner APIs. Live on DigitalOcean: pool min 0, max 1, size `s-2vcpu-4gb`, region nyc3.
1. Fill the control plane.
2. Create one more box: a droplet is created, the node joins, and the box becomes ready. Record the time.
3. Stop it: after `idleMinutes` (set to 3 for the test), the droplet is destroyed.
4. Resume the box: a new node is created and the box's data survives.
5. Delete everything. No orphan droplets are left (`doctl compute droplet list --tag-name boxes-pool-*`).
