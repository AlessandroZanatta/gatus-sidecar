# gatus-sidecar

> [!WARNING]
> **Alpha software, and written by an AI.**
>
> This is a `0.x` project. The annotation contract and the CRD schema can change
> in any release, including in ways that silently stop endpoints being monitored.

Keeps a [Gatus](https://github.com/TwiN/gatus) configuration in sync with your
cluster. Workloads declare their own monitoring through annotations, reusable
configuration lives in a CRD, and the sidecar renders a complete `config.yaml`
into a volume Gatus hot-reloads.

The problem it solves: a hand-maintained Gatus ConfigMap is a single file that
every team has to edit, sitting far away from the workloads it describes. YAML
anchors only deduplicate _within_ that one file, so the config drifts from
reality and grows without bound. This replaces it with per-workload annotations
plus cluster-scoped templates.

## How it works

```
Service ──┐
IngressRoute ──┼──► registry (unresolved) ──► render ──► atomic write ──► Gatus reload
Namespace ──┘                                    ▲
EndpointTemplate ────────────────────────────────┘
```

The registry holds _unresolved_ endpoints: which templates an endpoint wants,
not their contents. Templates are applied at render time, so editing a template
re-renders everything that uses it with no reverse index to maintain.

Each replica watches independently and writes its own local file, so there is no
leader election and nothing to coordinate.

## Quick start

```sh
kubectl apply -f config/crd/bases          # the EndpointTemplate CRD
kubectl apply -f deploy/rbac               # ServiceAccount, ClusterRole, binding
kubectl apply -f config/samples/endpointtemplates.yaml
kubectl apply -f deploy/gatus-deployment.yaml
```

Then annotate something:

```sh
kubectl annotate service/web -n storefront gatus.kalexlab.xyz/enabled=true
```

It appears on the status page within a second. No restart, no central file.

## Templates

`EndpointTemplate` is cluster-scoped and replaces YAML anchors. `extends`
composes templates; `defaultFor` makes one apply automatically to every endpoint
of a matching scheme, so workloads never name it.

```yaml
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: common-alerts
spec:
  endpoint:
    interval: 1m
    alerts:
      - type: telegram
      - type: telegram
        provider-override:
          id: "${TELEGRAM_SECONDARY_ID}"
---
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: default-http
spec:
  extends: [common-alerts]
  defaultFor: [http, https] # applies without being named
  scheme: http
  endpoint:
    conditions: ["[STATUS] == 200"]
```

`spec.endpoint` is passed through verbatim and never validated against Gatus's
schema, so fields added by future Gatus versions work without a CRD change.

Check what resolved:

```console
$ kubectl get endpointtemplates
NAME            SCHEME   DEFAULT FOR      USED BY   READY   AGE
common-alerts                             41        True    5m
default-http    http     ["http","https"] 33        True    5m
default-tcp     tcp      ["tcp"]          8         True    5m
```

A template that fails to resolve is reported `Ready=False` and skipped. It never
blocks the rest of the configuration from being written.

## Annotations

Every annotation is prefixed `gatus.kalexlab.xyz/`.

| Annotation       | Applies to                           | Meaning                                                    |
| ---------------- | ------------------------------------ | ---------------------------------------------------------- |
| `enabled`        | Service, IngressRoute                | `true` opts in, `false` opts out                           |
| `exclude`        | Service, IngressRoute                | Name globs; an object naming itself opts out               |
| `traefik-service`| IngressRouteTCP                      | `namespace/name`; picks which Traefik publishes it          |
| `name`           | Service, IngressRoute                | Endpoint name. Default: sentence-cased object name         |
| `group`          | Service, IngressRoute, **Namespace** | Group. Empty string means _no_ group                       |
| `template`       | Service, IngressRoute                | Comma list; **replaces** the `defaultFor` selection        |
| `template-extra` | Service, IngressRoute                | Comma list; **appended after** it                          |
| `scheme`         | Service, IngressRoute                | `http`/`https`/`tcp`; also selects which templates apply   |
| `port`           | Service                              | Port name or number. Required only when >1 port            |
| `path`           | Service, IngressRoute                | Appended to the derived URL                                |
| `url`            | Service, IngressRoute                | Full override; skips all derivation                        |
| `endpoint`       | Service, IngressRoute                | Raw YAML merged last — the escape hatch                    |
| `endpoints`      | Service, IngressRoute                | Raw YAML **list**; emits several endpoints from one object |

### Group inheritance

Annotate the namespace once instead of every workload in it:

```yaml
kind: Namespace
metadata:
  name: storefront
  annotations:
    gatus.kalexlab.xyz/group: Online Shop
```

Precedence is object → namespace → sentence-cased namespace name. An **empty**
`group` annotation means the endpoint sits at the top level, which is different
from an absent one.

### Excluding objects annotated in bulk

Anything that propagates annotations to a family of objects at once — an
operator copying metadata onto every Service it creates, a Helm chart's shared
`commonAnnotations` — makes `enabled` all-or-nothing. `exclude` is a list each
copy carries, and every object checks it against **its own name**: entries
naming something else are ignored, so one shared value suppresses only the
members it names.

```yaml
gatus.kalexlab.xyz/enabled: "true"
gatus.kalexlab.xyz/exclude: db-replicas, *-headless
```

Applied to `db-primary`, `db-replicas` and `db-headless`, that leaves only
`db-primary` monitored, without any per-object annotation.

Entries are separated by commas or newlines and are `path.Match` globs, so
`*-headless` covers a naming convention, `db-?` a single character, `db-[rn]o`
a set, and a bare `*` everything. An unparseable pattern matches
nothing rather than everything.

Exclusion beats `enabled`: the annotation that turned the family on is the one
every member inherited, so overriding it per object is the whole point.

### Several endpoints from one Service

Items inherit the object's other annotations and override what they name:

```yaml
gatus.kalexlab.xyz/enabled: "true"
gatus.kalexlab.xyz/group: Platform
gatus.kalexlab.xyz/endpoints: |
  - name: Object store
    port: 9000
    path: /health
  - name: Object store console
    port: 9001
```

### Overriding one field

```yaml
gatus.kalexlab.xyz/enabled: "true"
gatus.kalexlab.xyz/endpoint: |
  conditions:
    - "[STATUS] > 400"
    - "[STATUS] < 500"
```

## Merge rules

Lowest to highest precedence:

1. Templates selected by `defaultFor`, or by `template` if set, each with its
   `extends` chain flattened first (depth-first, left to right)
2. `template-extra`, appended after
3. Identity: `name`, `group`, `url` — a template can never rename what is checked
4. `endpoint` / `endpoints` raw patch

**Maps merge recursively; scalars and lists replace.** List-replace is what makes
the `conditions` override above behave as written rather than accumulating the
template's condition plus yours.

`${VAR}` placeholders pass through untouched — Gatus expands them, not the
sidecar, which never reads or logs their values.

## IngressRoutes

A Traefik IngressRoute yields **two** endpoints: the public address from the
`Host()` matcher, and the in-cluster address of the backing Service. The external
one exercises DNS, TLS, the proxy and any middleware; the internal one isolates
the workload. When they disagree, the difference is the signal.

Match rules are parsed with Traefik's own parser, so precedence, grouping and
negation are handled correctly — `!Host(\`internal.example.org\`)` describes an
address the route deliberately does not serve, and is not monitored.

When a Service is annotated and an IngressRoute points at it, the in-cluster
endpoint inferred from the route is dropped: same address, and the annotated
Service is the deliberate statement of the two. They are compared by host and
port rather than by URL, so a health path on the Service does not defeat it. The
route's public endpoint always stays — nothing else covers DNS, TLS and the
proxy.

## IngressRouteTCP

A Traefik TCP router yields the same two endpoints, but its public address takes
more work: the router names an **entrypoint**, not a port, and only the Traefik
Service knows what that entrypoint was published on. The chart names each
Service port after its entrypoint, so the port is discovered by joining the two.

```yaml
kind: IngressRouteTCP
metadata:
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  entryPoints: [mqtt]          # -> traefik Service port named "mqtt" -> 8883
  routes:
    - match: HostSNI(`mqtt.example.org`)
      services:
        - name: mosquitto
          port: 1883
  tls:
    secretName: example-org-tls
```

becomes `tls://mqtt.example.org:8883` and `tcp://mosquitto.ns.svc:1883`. The
scheme follows the router: `tls` when Traefik terminates TLS, so the check
exercises the certificate rather than only the socket, and `tcp` when it does
not. The in-cluster endpoint is always `tcp` — the backend is a plain socket
whatever sits in front of it.

Several Traefik installations are supported, and expected: splitting internal
from external traffic gives two Services, each publishing its own ports. Every
Service labelled `app.kubernetes.io/name=traefik` is read, or exactly the ones
named by `--traefik-service`. When two installations publish the same entrypoint
on **different** ports, the route says which it belongs to:

```yaml
gatus.kalexlab.xyz/traefik-service: traefik/external
```

Without that, the ambiguity is reported and the route is skipped rather than
monitored at a guessed address. Installations agreeing on a port are not
ambiguous and need nothing. A NodePort installation contributes its node port,
since that is what a client outside the cluster connects to; anything else —
host ports, unusual port names — is what `--entrypoint-port mqtt=8883` is for.

A router matching `HostSNI(`*`)` serves whatever reaches the entrypoint and names
no address, so only its in-cluster endpoint is produced.

## Base configuration

Settings that are genuinely singletons — `security`, `storage`, `alerting`
providers, `ui`, `connectivity` — stay in a ConfigMap passed as `--base-config`.
The sidecar merges the generated `endpoints` list into it and passes everything
else through byte for byte. Any `endpoints` key in the base is ignored, since
the sidecar owns that list.

## Flags

| Flag                          | Default               |                                                  |
| ----------------------------- | --------------------- | ------------------------------------------------ |
| `--base-config`               | _(none)_              | Operator-maintained part of the config           |
| `--output`                    | `/config/config.yaml` | Where to write                                   |
| `--service-discovery`         | `opt-in`              | `opt-in`, `auto`, `disabled`                     |
| `--ingressroute-discovery`    | `opt-in`              | Independent of the above                         |
| `--ingressroutetcp-discovery` | `opt-in`              | Traefik TCP routers                              |
| `--traefik-service`           | _(auto)_              | `namespace/name` list supplying entrypoint ports |
| `--entrypoint-port`           | _(auto)_              | `entrypoint=port` overrides                      |
| `--namespace-selector`        | _(all)_               | Label selector limiting which namespaces count   |
| `--external-suffix`           | `" (external)"`       | Distinguishes the IngressRoute's public endpoint |
| `--cluster-domain`            | `cluster.local`       |                                                  |
| `--default-scheme`            | `http`                | When neither workload nor template says          |
| `--group-from-namespace`      | `true`                | Derive a missing group from the namespace        |
| `--debounce`                  | `500ms`               | Quiet period before rendering                    |
| `--metrics-bind-address`      | `:8081`               | `0` disables                                     |
| `--health-probe-bind-address` | `:8082`               |                                                  |

`auto` mode monitors every object unless it sets `enabled=false`. Expect noise:
headless services, operator webhooks and metrics ports all become endpoints.
`opt-in` is the default for that reason.

## Behaviour worth knowing

- **The first configuration written is already complete.** Gatus deletes the
  stored history of every endpoint missing from a configuration it reloads, so
  publishing a file while the registry is still filling in destroys history for
  whatever has not reconciled yet. The sidecar waits for its caches, primes the
  registry from a full listing, and only then writes.
- **Writes are atomic and skipped when unchanged.** Gatus reloads on file change,
  and a reload restarts every check's interval, so a no-op reconcile must not
  touch the file.
- **Skip warnings are logged when they change, not on every render.** A render
  happens per watch event, and an operator that rewrites its own Services stays
  noisy indefinitely.
- **A failed render leaves the previous file in place.** A stale configuration
  still monitors things; an empty one does not.
- **A malformed annotation drops that one endpoint**, logs why, and lets the rest
  render. It is not retried, because it is not a transient failure.
- **Duplicate `(group, name)` pairs are dropped with a warning**, since Gatus keys
  stored history on that pair and a collision would interleave two services'
  results.
- **Not ready until a config has been written**, so a rollout does not retire the
  old pod while the new one has nothing for Gatus.

## Metrics

`gatus_sidecar_endpoints`, `_sources`, `_render_warnings`,
`_render_errors_total`, `_last_successful_render_timestamp_seconds`.

Alert on `_render_warnings > 0` (part of the intended configuration is silently
missing) and on `_last_successful_render_timestamp_seconds` going stale (the
file on disk is drifting from the cluster).

## RBAC

Read-only on `services`, `namespaces`, `traefik.io/ingressroutes` and
`endpointtemplates`. The only write is to `endpointtemplates/status`.

## Development

```sh
make help          # list targets
make ci            # generated-file drift, formatting, vet, build, unit tests
make test-e2e      # creates a kind cluster, runs against a real API server, deletes it
make test-e2e-keep # same, but leaves the cluster for inspection
make docker-build
```

The e2e suite installs the real Traefik IngressRoute CRD (vendored in
`test/e2e/testdata`, refresh with `make update-traefik-crd`) so fixtures are
validated the way a real cluster would validate them.

Commits follow [Conventional Commits](https://www.conventionalcommits.org).
release-please derives the version and `CHANGELOG.md` from them and opens a
release PR; merging it tags the release and pushes the image to
`ghcr.io/alessandrozanatta/gatus-sidecar`.
