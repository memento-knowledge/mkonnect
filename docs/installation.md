# Installing the connector

This guide walks a customer administrator through deploying the mkonnect connector inside their
network and confirming it has connected to the Memento platform. Once it is running, follow the
[Setup guide](setup.md) to make your internal tools reachable.

For a conceptual overview and the full environment-variable reference, see the [README](../README.md).

## Before you start

**What the connector needs**

- **Outbound HTTPS (443) only.** The connector dials *out* to the Memento Bridge Gateway and to
  Memento's container registry. It never accepts inbound connections, so no inbound firewall rules
  or port-forwarding are required. It also needs outbound access to whichever internal tools you
  want to bridge (e.g. Jenkins, Prometheus) on their own ports.
- **A runtime:** Docker Engine, or a Kubernetes cluster with Helm 3.8+ (OCI support).
- **Persistent storage** for the connector's private key (a Docker volume or a
  `PersistentVolumeClaim`). The key is issued once at registration and reused on every reconnect;
  losing it means re-registering.

**What you'll collect from the Memento portal**

Open the Memento portal, go to **Integrations → On-Prem Connector**, and note:

| Value | Used as | Notes |
|---|---|---|
| Gateway URL | `GATEWAY_URL` | `wss://<customer-slug>.bridge.memento-platform.com/ws` |
| Connector ID | `CONNECTOR_ID` | a UUID identifying this connector |
| Registration token | `REGISTRATION_TOKEN` | **one-time**, expires in 24h — used only on first start |
| Release ID / chart version | image tag / `--version` | pin an explicit version; never a moving tag |

The registration token is a one-time secret. It is consumed on first startup; after that the
connector authenticates with the private key it was issued and the token is no longer needed.

> **Security note:** `GATEWAY_URL` must be `wss://` (TLS). The connector refuses a plaintext
> `ws://` gateway unless you explicitly set `ALLOW_INSECURE_GATEWAY=true` *and* the host is
> loopback — an escape hatch intended only for local development.

## Option A — Docker

Create a named volume for the key, then start the connector pinned to a specific release ID
(from the portal, e.g. `20260919.0`):

```bash
docker volume create mkonnect-data

docker run -d --name mkonnect --restart unless-stopped \
  -e GATEWAY_URL=wss://<customer-slug>.bridge.memento-platform.com/ws \
  -e CONNECTOR_ID=<uuid> \
  -e REGISTRATION_TOKEN=<token> \
  -e KEY_FILE=/data/key \
  -v mkonnect-data:/data \
  public.ecr.aws/h2a8k0r3/memento-connector:<release-id>
```

The image is a distroless, non-root, statically linked binary. The `-v mkonnect-data:/data`
volume is where the private key (`/data/key`) and the local credential store
(`/data/credentials.json`) live, so both survive container restarts and upgrades.

You can define unauthenticated internal tools inline with `PLUGIN_<NAME>` variables (e.g.
`-e PLUGIN_PROMETHEUS=http://prometheus.internal:9090`); authenticated tools are configured after
startup — see the [Setup guide](setup.md).

## Option B — Kubernetes (Helm)

The chart is published as an OCI artifact. The registration token is a secret, so pass it through
a values file rather than `--set` (which would land in your shell history and the process list):

```bash
cat > mkonnect-secrets.yaml <<'EOF'
config:
  registrationToken: "<token>"
EOF

helm install mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> \
  -f mkonnect-secrets.yaml \
  --set image.repository=public.ecr.aws/h2a8k0r3/memento-connector \
  --set config.gatewayUrl=wss://<customer-slug>.bridge.memento-platform.com/ws \
  --set config.connectorId=<uuid>
```

The chart writes `registrationToken`, `gatewayUrl`, and `connectorId` into a Kubernetes `Secret`,
and provisions a `PersistentVolumeClaim` so the private key survives pod restarts. See
[`charts/mkonnect/values.yaml`](../charts/mkonnect/values.yaml) for all options (resources,
storage class, plugins, service account).

After the connector has registered once, delete the local `mkonnect-secrets.yaml`. Note that
deleting it does **not** scrub the token from the cluster — it remains in the deployed `Secret`
and in Helm's release metadata. Restrict access to both (RBAC on `secrets` and on
`helm get values`), and clear the token explicitly when it is no longer needed:

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <chart-version> --reuse-values \
  --set config.registrationToken=""
```

## Verify the connection

The connector should register and connect within a few seconds.

- **Portal:** the On-Prem Connector tab shows **bridge status: connected**, along with the
  connector version and a recent heartbeat.
- **Docker logs:**

  ```bash
  docker logs mkonnect
  ```

  A healthy start logs `mkonnect starting — connector=… gateway=… protocol=… version=…` and then
  no repeating `gateway connection failed` warnings.
- **Inside the container:**

  ```bash
  docker exec -i mkonnect connector status          # Docker
  kubectl exec -i <pod> -- connector status         # Kubernetes
  ```

If the status stays **degraded**, see [Troubleshooting](#troubleshooting).

Next: [set up your internal tools](setup.md).

## Upgrading

Releases use calendar identifiers (`YYYYMMDD.n`). Always pin an explicit release; never track a
moving tag. Each Helm chart version's default image tag is its `appVersion`, so choosing the chart
version selects the matching image.

**Docker:** pull the new release ID and recreate the container with the same volume:

```bash
docker pull public.ecr.aws/h2a8k0r3/memento-connector:<new-release-id>
docker rm -f mkonnect
docker run -d --name mkonnect --restart unless-stopped \
  ... (same flags as install) \
  public.ecr.aws/h2a8k0r3/memento-connector:<new-release-id>
```

Because the key and credential store live on the `mkonnect-data` volume, the connector reconnects
with its existing key — no re-registration and no credential re-entry.

**Helm:**

```bash
helm upgrade mkonnect oci://public.ecr.aws/h2a8k0r3/mkonnect \
  --version <new-chart-version> --reuse-values

# roll back a bad upgrade:
helm history mkonnect
helm rollback mkonnect <revision>
```

Each GitHub Release also carries the chart `.tgz` and a `.sha256` checksum; if you install from the
downloaded archive instead of OCI, verify it first with `sha256sum -c <chart>.tgz.sha256`.

## Uninstalling

**Docker:**

```bash
docker rm -f mkonnect
docker volume rm mkonnect-data     # also removes the private key and local credentials
```

**Helm:**

```bash
helm uninstall mkonnect
# the PersistentVolumeClaim may be retained by your storage class; delete it explicitly
# if you want to discard the key:
kubectl delete pvc mkonnect-data
```

Because credentials for your internal tools are stored **only** in the connector's local volume
(see the [Setup guide](setup.md)), removing the connector and its volume removes them from your
network entirely — there is nothing to revoke on Memento's side.

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| Container exits immediately with a config error | A required variable is missing or malformed. `GATEWAY_URL` must be an absolute `wss://` URL and `CONNECTOR_ID` must be set. Check `docker logs mkonnect`. |
| `GATEWAY_URL must use wss://` | The gateway URL is plaintext `ws://`. Use `wss://`. For local development only, a loopback `ws://` URL is allowed with `ALLOW_INSECURE_GATEWAY=true`. |
| Repeating `gateway connection failed` warnings | The connector can't reach the gateway. Confirm outbound HTTPS (443) to `<customer-slug>.bridge.memento-platform.com` is allowed by your firewall/proxy. The connector retries with backoff (1s → 60s), so transient failures self-heal. |
| Portal shows **degraded** after it was connected | The connector lost its heartbeat (stopped, or lost outbound access). Verify the container/pod is running and can still reach the gateway. |
| First start fails with a registration error | The registration token is expired (24h) or already consumed. Generate a fresh token in the portal and restart. |
| `key file has unsafe permissions` | The persisted key file is group/world-accessible. The connector requires owner-only (`0600`) access; fix the volume's permissions. |

For issues configuring the tools the connector proxies (auth failures, unreachable services), see
the [Setup guide's troubleshooting](setup.md#troubleshooting).
