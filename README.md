# datumctl-compose

[![CI](https://github.com/dlotterman/datumctl-compose/actions/workflows/ci.yml/badge.svg)](https://github.com/dlotterman/datumctl-compose/actions/workflows/ci.yml)

> [!WARNING]
> Datum Compute is in an invite-only preview. This project will stay in **alpha** until Compute is officially released and a few more features are in place. Expect breaking changes.

Deploy a Docker Compose application to [Datum Compute](https://datum.net) with `datumctl compose`.

This is a `datumctl` plugin. It reads a normal Compose file and turns each service into a Datum workload, with private networking and optional public HTTP through Datum's load balancer.

## Install

```sh
go build -o datumctl-compose ./cmd/datumctl-compose
# Move datumctl-compose somewhere on your PATH, then:
datumctl plugin trust compose
```

If you rebuild the binary, run `datumctl plugin trust compose` again. Check the installed version with `datumctl compose version`.

## Quick start

```sh
# Preview the Datum resources without changing anything
datumctl compose config --location us-west-1 -f compose.yaml

# Deploy
datumctl compose up --location us-west-1 -f compose.yaml

# Check status
datumctl compose ps -f compose.yaml

# Tear it all down
datumctl compose down -f compose.yaml
```

`up` doesn't wait for workloads to become ready. Use `ps` to see their progress.

## What gets created

For a Compose project named `myapp`, each service becomes:

| Compose | Datum resource | Name |
| --- | --- | --- |
| A service | `Workload` | `myapp-<service>` |
| A service with `ports` | Private `NetworkService` | `myapp-<service>` |
| A service with `http-port` | Public `HTTPProxy` | `myapp-<service>` |
| A network | `Network` | `myapp-<network>` |

If the Compose file has no `name:` and you don't pass `-p`, the project name comes from the current directory. Long or unusual names are shortened and get a short hash suffix.

The plugin labels everything it creates. It won't touch resources owned by another project or tool, and `down` deletes only what it owns.

## Common flags

Run `datumctl compose <command> --help` to see every flag.

| Flag | What it does |
| --- | --- |
| `--location` | Datum location to deploy to (required for `config` and `up`) |
| `-f` | Compose file. Repeat it to add override files |
| `-p` | Compose project name |
| `--profile` | Enable a Compose profile |
| `--datum-project` | Datum project to deploy into |
| `--network NAME` | Put every service on one existing Datum network |
| `--runtime-class`, `--instance-type` | Change the compute type (the default is `general-purpose`) |
| `--best-effort` | Skip unsupported Compose fields and print a warning for each one instead of failing |
| `-v`, `--verbose` | Print each `datumctl` command and manifest the plugin runs |

The plugin also reads `.env`, `COMPOSE_FILE`, `COMPOSE_PROFILES`, and override files the same way Docker Compose does.

## Making a service public

Add `http-port` to a service. It's a `datumctl compose` extension, not a standard Compose field. The value must match one of the service's `ports`:

```yaml
services:
  web:
    image: docker.io/library/nginx:1.27
    ports:
      - target: 80
    http-port: 80
```

Datum serves HTTPS at its load balancer and forwards plain HTTP to your container. Your container must listen on IPv6.

To find the public hostname, run:

```sh
datumctl get httpproxy <project>-web -o yaml
```

DNS can take a few minutes to start working. To make a service private again, remove `http-port` and run `up` again.

Services without `http-port` stay private. Published host ports such as `8080:80` do **not** make a service reachable from the internet.

See [Datum's publishing guide](https://datum.net/docs/compute/publish-workloads) for more.

## Networks

Each Compose network becomes a Datum network. Services that don't list any networks share one called `<project>-default`.

```yaml
networks:
  frontend: {}
  backend: {}
services:
  web:
    image: docker.io/library/nginx:1.27
    networks: [frontend]
  db:
    image: docker.io/library/postgres:16
    networks: [backend]
```

Things to know:

- **One network per service.** A Datum instance can only be on one network, so a service that lists two networks is an error.
- **Separate networks are isolated.** Services on different networks can't reach each other.
- **No service DNS.** Services can't find each other by name the way they can in Docker Compose.
- **External networks.** If a network has `external: true`, the plugin uses an existing Datum network with that name and never deletes it.
- **Removed networks.** If you remove a network from the Compose file, `up` keeps it and `down` deletes it.

## Supported Compose fields

The plugin supports these service fields:

- `image` (must include a registry, such as `docker.io/library/nginx:1.27`)
- `environment` and `env_file`
- `entrypoint` and `command`
- `ports` (container ports only)
- `scale` and `deploy.replicas`
- `networks`

Any other field, such as `volumes`, `build`, `depends_on`, `healthcheck`, or `secrets`, makes the command fail and tells you which field caused it. Use `--best-effort` to skip those fields and print a warning for each one. Check the `config` output before you run `up` with `--best-effort`.

The plugin doesn't build images. Push your image to a registry first.

## Limitations

- **No persistent storage.** Data is lost when you run `down` or when an instance is replaced.
- **Environment variables are visible.** Values, including passwords, are stored in the workload spec. Don't use real production secrets yet.
- **No startup ordering.** `depends_on` isn't supported.
- **Partial failures.** If `up` fails partway through, any changes it already made stay in place.

## Examples

- [`examples/http-port.compose.yaml`](examples/http-port.compose.yaml): one public service and one private service on separate networks.
- [`examples/nginx-postgres.compose.yaml`](examples/nginx-postgres.compose.yaml): Nginx and Postgres on a shared private network. Set `POSTGRES_PASSWORD` before you run any command.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

The tests run against a fake `datumctl`, so they don't need Datum credentials and don't touch real resources. They don't check that Datum's API accepts the generated resources.

| Package | Responsibility |
| --- | --- |
| `cmd/datumctl-compose` | CLI commands and flags |
| `internal/compose` | Loads Compose files, `.env`, profiles, and `http-port` |
| `internal/translate` | Converts Compose services and networks into Datum resources |
| `internal/datum` | Datum resource types and the `datumctl` client |
| `internal/identity` | Resource naming and ownership labels |
| `internal/deploy` | `up` and `down`: ownership checks, create/update, delete |

A command runs through them in this order: `compose.Load` → `translate.Translate` → `deploy.Deployer`, which calls `datumctl` through `datum.Client`.
