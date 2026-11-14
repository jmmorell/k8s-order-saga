# ADR 0005: kind as the local Kubernetes cluster

## Status

Accepted

## Context

Day 2 moves the five services from Docker Compose to Kubernetes. We need a local cluster that is disposable, close to a real multi-node cluster, and scripted from zero so anyone can rebuild it in minutes.

## Decision

- Use kind with one control-plane and two workers, named `order-saga` (context `kind-order-saga`). Every script passes the context explicitly.
- The control-plane maps NodePort 30080 to `127.0.0.1:8080`; this is the only way in from the host.
- Platform components run in namespace `platform`: NATS from the official `nats/nats` chart (version pinned in `deploy/local/versions.env`, currently 2.15.2), and Postgres 17 and Mailpit v1.33.1 as plain manifests.
- Postgres is a single-replica StatefulSet with a 1Gi PVC, not an operator. Locally, durability and failover do not matter, and a StatefulSet is the easiest thing to read and explain.
- `make local-up` is idempotent; the database password is generated on first run into a Secret and never stored in Git.

## Alternatives considered

- k3d: fast and light, but k3s differs from upstream Kubernetes (bundled Traefik, other defaults).
- minikube: works everywhere, but multi-node and port mapping are clumsier and slower.
- OrbStack's built-in Kubernetes: very quick, but single node and not disposable per project.

## Consequences

- Nodes are Docker containers, so the cluster shares the machine's memory; the platform pods are sized to keep the whole cluster under 6 GB.
- Differences from GKE: no cloud load balancer (a NodePort plus port mapping replaces it), the default `standard` StorageClass uses local-path volumes on the node, no managed database (Cloud SQL would replace the StatefulSet), and images are loaded with `kind load` instead of pulled from a registry.
- Compose and kind both use host port 8080, so only one can run at a time; `local-up.sh` fails early if the port is taken.
