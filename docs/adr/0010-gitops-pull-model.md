# ADR 0010: GitOps with Argo CD (pull model)

## Status

Accepted

## Context

Until day 4, `make local-up` installed the five services and `charts/saga-observability` with `helm upgrade --install` from the working copy, and applied the NetworkPolicies with `kubectl apply`. What ran in the cluster was whatever the last person to run the script had on disk. Day 4 adds a release workflow (`.github/workflows/release.yml`, Task 4) that builds and scans the images, pushes them to GHCR and commits the new image tag to `deploy/local/values/<service>.yaml`. Something has to turn that commit into a rollout, and the cluster has to converge on Git rather than on a laptop.

## Decision

### Pull, not push: CI never gets cluster credentials

Argo CD runs inside the cluster and pulls. It watches the repository, renders what Git says and applies the difference. CI's job ends at a Git commit.

- **Push** (CI runs `helm upgrade` or `kubectl apply`) needs a kubeconfig or a cloud identity with write access to the cluster stored in GitHub. Anyone who can change a workflow, or a compromised third-party action, gets that access. The cluster's API server must also be reachable from GitHub's runners, which a kind cluster on a laptop is not and a private GKE control plane should not be.
- **Pull** keeps every credential inside the cluster. The release workflow holds only `packages: write` for GHCR and `contents: write` for the tag commit. The worst a compromised workflow can do is commit a bad tag, which is visible in Git and undone with `git revert` (the bad-rollout drill below).
- Git is the only way in. A change made with `kubectl` behind Argo CD's back is reverted (`selfHeal`), and an object removed from Git is deleted (`prune`). The cluster can be rebuilt from the repository plus `make local-up`.

### The manual trigger

A release is `gh workflow run release.yml --ref main` (`workflow_dispatch`), not every push to `main`. A portfolio project has no traffic that needs continuous delivery, and an explicit trigger makes "what is deployed" a decision someone took. It also keeps the GHCR packages and the tag commits on `main` meaningful. Argo CD does not care how the commit arrived: a tag edited by hand and pushed is deployed the same way, so the CI tag-bump step can be cut without losing GitOps.

### Bootstrap versus workload

`scripts/local-up.sh` still installs the platform with Helm: the namespaces and Secrets, Postgres, NATS, Mailpit, metrics-server, kube-prometheus-stack, Tempo, Loki, the OTel Collector, Envoy Gateway and Argo CD itself. Argo CD deploys the workload through an app-of-apps:

- `deploy/argocd/root.yaml` is the root Application. `local-up.sh` applies it with its revision set from `ARGOCD_REVISION` (default `main`), so a branch can be tested on the cluster before it is merged.
- `deploy/argocd/apps` is a small chart that renders the seven children: `svc-<name>` for each service (`charts/service` with `deploy/local/values/<name>.yaml`, namespace `shop`), `saga-observability` (namespace `observability`) and `policies` (`deploy/local/policies`, every namespace's NetworkPolicies).
- Every Application syncs automatically with `prune` and `selfHeal`, and none creates a namespace. The namespaces carry Pod Security labels (ADR 0009) and hold Secrets that `local-up.sh` creates, so they belong to the bootstrap.

The platform charts stay in the script this iteration because of ordering. kube-prometheus-stack installs the CRDs (PrometheusRule, ServiceMonitor, PodMonitor) that NATS and `saga-observability` use. Envoy Gateway installs the Gateway API CRDs and controller that the `shop` Gateway needs. Postgres and NATS must be up before a service passes its startup probe. In Argo CD that means sync waves across Applications, `SkipDryRunOnMissingResource`, and server-side apply for CRDs too large for the last-applied annotation. It is all doable, but none of it is needed to show the pull model on the workload, which is the part that changes with every release. Moving the platform charts into Argo CD, one Application each, ordered with sync waves, is roadmap.

The Helm releases of the services from day 2 and 3 are not adopted. Argo CD renders charts with `helm template` and applies the result, so it never owns a Helm release. `local-up.sh` stops with a message asking for `make local-down && make local-up` once if it finds one.

### Self-heal and `make local-reload`

`selfHeal` reverts a field that Git sets: `kubectl scale --replicas=1` on `orders` is put back to 2 within the next reconciliation. A field that Git does not set is not drift. `kubectl rollout restart` only adds the `kubectl.kubernetes.io/restartedAt` annotation to the pod template. Argo CD's diff ignores fields that are absent from the desired manifest and from the last applied configuration, so the restart stays and `make local-reload` (rebuild the `dev` images, `kind load`, restart) keeps working with Argo CD in charge.

`build_and_load_images` builds and loads only the services whose values tag is `dev`. A service whose values carry a release SHA is pulled from GHCR by the nodes. Loading local code under a release tag would make the cluster run something other than what that tag names.

### Public repository and registry

The repository and the GHCR images are public, so Argo CD needs no repository credentials, and the nodes need no image pull Secret. Nothing in Git is a secret: the Postgres and Grafana admin passwords are generated by `local-up.sh` into Secrets that only exist in the cluster, and Argo CD's own admin password and Redis password are generated by Argo CD at install time. A private repository would need a deploy key or a GitHub App in an Argo CD repository Secret, created outside Git or through an external secret store.

### The Argo CD install

- Chart `argo-cd` from `https://argoproj.github.io/argo-helm`, pinned by `ARGOCD_CHART_VERSION` in `deploy/local/versions.env`, values in `deploy/local/values/argo-cd.yaml`.
- Only the application controller, the repo server, the API server and Redis run. Dex, the notifications controller and the ApplicationSet controller are not used. The chart has no switch for the ApplicationSet controller, so it gets zero replicas.
- Every container has requests and limits. The server's Service stays ClusterIP. The UI is reached with `make argocd`, which port-forwards to `https://localhost:8443` and prints the command that reads the initial admin password.
- The chart's own NetworkPolicies are turned off. The `argocd` namespace uses ours (`deploy/local/policies/argocd.yaml`): ingress default-deny with same-namespace traffic allowed, and egress open for GitHub and the API server. `kubectl port-forward` does not pass through the pod network, so it needs no allow. The namespace enforces Pod Security `baseline` and warns at `restricted` (ADR 0009).

### On GKE

- Argo CD is installed the same way, by the bootstrap, on a cluster Terraform created. Terraform never holds the workload either.
- The values files gain a GKE overlay (for example, `deploy/gke/values/<service>.yaml` for Cloud SQL and Workload Identity), and the apps chart takes the value file list as a value, so the same chart renders either environment.
- The API server can stay private: nothing outside the cluster needs to reach it to deploy. Argo CD's UI is still reached with `kubectl port-forward`, or with IAP, never on a public IP.
- Images are pulled from GHCR as on kind. Artifact Registry with Workload Identity would remove the dependency on a public registry.

## Alternatives considered

- **CI pushes with `helm upgrade`.** Rejected for the credentials and reachability reasons above.
- **Flux.** It is equivalent for this use. Argo CD's UI shows the sync and health of each Application, which is what the bad-rollout drill and the tour need to show.
- **ApplicationSet instead of an app-of-apps chart.** It would generate the five service Applications from a list or a Git directory generator. It is one more controller running to save a few lines of template, and the explicit chart is easier to read and test offline (`scripts/chart-test.sh` asserts the seven Applications).
- **Everything in Argo CD now.** Deferred for the CRD ordering above.

## Consequences

- To deploy, commit and push. `ARGOCD_REVISION=<branch> make local-up` points the cluster at a branch.
- A bad commit is rolled back with `git revert`. The bad-rollout drill sets a payments tag that does not exist. The new ReplicaSet's pod sits in `ErrImagePull` while the old pod keeps serving (the default rolling update has `maxUnavailable` 25%, which rounds down to 0 for one replica), and the `svc-payments` Application shows Progressing, then Degraded once the Deployment's progress deadline passes. Reverting the commit returns it to Healthy.
- Argo CD adds four pods and about 300 MiB to the cluster, within the 6 GB budget.
- A cluster from day 2 or 3 must be recreated once.
- The platform is still installed imperatively. Upgrading a platform chart is a `versions.env` change plus `make local-up`, not a Git-driven sync.
