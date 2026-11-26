# Day 4 — Security and GitOps: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The kind cluster is locked down and deployed from Git.
- Pods are hardened: Pod Security `restricted`, no ServiceAccount tokens.
- Networking is default-deny, with explicit allows.
- Argo CD syncs the workload from the now-public GitHub repo.
- A manually triggered GitHub Actions workflow builds, scans and pushes the images and commits the new tag; Argo CD then rolls it out.

**Architecture:** Two layers.
- **Bootstrap** stays in `scripts/local-up.sh`: the cluster, namespaces with their Pod Security labels, the Secrets, the platform and observability charts (which carry CRDs), and Argo CD itself.
- **Workload** is what Argo CD owns: the five services, `charts/saga-observability` and the NetworkPolicies, through one root Application (app-of-apps).
- CI never touches the cluster. It pushes images to GHCR and commits the new tag to the values files, and Argo CD pulls.

**Tech Stack:** Pod Security Admission, NetworkPolicy (kindnet), Argo CD (Helm chart), GitHub Actions, Docker Buildx (multi-arch), Trivy, GHCR, gitleaks, actionlint.

**Spec:** `docs/superpowers/specs/2026-11-12-k8s-portfolio-design.md` (§2, §6 "Security" and "GitOps", §7 day 4)

## Global Constraints

- Everything in the repo is in **English**.
- Every `kubectl`/`helm`/`argocd` call in scripts and the Makefile passes `--context kind-order-saga` (`--kube-context` for helm).
- Only `orders` is reachable from outside. Argo CD, like Grafana and the others, is reached only with `kubectl port-forward`.
- **Pod Security:** `shop` and `platform` enforce `restricted`. `observability`, `argocd` and `envoy-gateway-system` enforce `baseline` and warn and audit at `restricted`. Every namespace label is pinned to version `v1.37`.
- **ServiceAccounts:** one per service, with no API permissions and `automountServiceAccountToken: false` on both the ServiceAccount and the pod.
- **NetworkPolicies:**
  - `shop` and `platform`: default-deny for both ingress and egress, plus explicit allows (the traffic matrix in Task 3).
  - `observability` and `argocd`: default-deny ingress, with egress left open (ADR 0009 explains why).
  - DNS egress to `kube-system` on port 53, TCP and UDP, is allowed wherever egress is denied.
- **No secrets in Git.** Argo CD needs no repository or registry credentials, because the repo and its GHCR images are public. The `postgres` and `grafana-admin` Secrets stay created by `local-up.sh`.
- **Image references:**
  - Every service image is `ghcr.io/jmmorell/k8s-order-saga/<service>:<tag>`. The tag lives in `deploy/local/values/<service>.yaml` (`image.tag`). Its initial value is `dev`; CI later replaces it with the short commit SHA.
  - `local-up.sh` builds and `kind load`s the images under exactly that reference, so the cluster never needs the registry for a locally built tag.
- **GitHub Actions:** every third-party action is pinned to a full commit SHA, with the version in a comment. Workflows get the least `permissions:` they need (default `contents: read`).
- **Pinned versions:** the Argo CD chart, Trivy, gitleaks and the actionlint versions go in `deploy/local/versions.env` or in the workflow, never `latest`.
- **Unchanged behaviour:** `make local-up` stays idempotent. `make local-smoke` prints `SMOKE OK`, `make obs-check` prints `OBS OK`, and `scripts/trace-check.sh` passes after every task. `make lint` reports 0 issues, `go test -race ./...` passes, `make chart-test` prints `CHART OK`, and Compose `make up && make smoke` still works.
- **Making the repo public is the user's action**, taken after Task 1's secret scan is clean. The controller stops and asks before it happens.

## Review Focus

1. **Kubelet probes and `kubectl port-forward` under default-deny.** Probes must keep passing, and `local-smoke` (which port-forwards Mailpit) and `trace-check` must keep working. Task 3 runs all of them after applying the policies, and records whether kindnet exempts node-originated traffic.
2. **An image tag commit lands on `main` while a pod is unhealthy.** The rollout must stall safely, with the old ReplicaSet still serving, and Argo CD must show Degraded rather than hiding it. Task 5 checks this with a tag that does not exist.
3. **Someone edits a Deployment with `kubectl` behind Argo CD's back.** Argo CD self-heals back to Git, and the tour explains it. Checked in Task 5.
4. **CI is triggered twice, or the tag-bump commit races another push to `main`.** The bump must not overwrite unrelated changes and must not loop by triggering CI again. Checked in Task 4 (bump script test and workflow design).
5. **A pod or a probe that `restricted` would reject.** It is caught at `kubectl apply` or `helm` time with a clear error, not as a silent CrashLoop. Task 2 does a server-side dry-run of a privileged pod in `shop` and `platform`.

---

## File Structure

```
.github/workflows/
  ci.yml                     # every push and PR: lint, test, chart-test, rules-test
  release.yml                # PR: build + scan only; workflow_dispatch: build, scan, push, bump tag
scripts/
  secrets-scan.sh            # gitleaks over the full history
  policy-test.sh             # Pod Security + NetworkPolicy probes against the live cluster
  bump-image-tag.sh          # rewrites image.tag in deploy/local/values/*.yaml
  bump-image-tag_test.sh
deploy/local/policies/{shop,platform,observability,argocd}.yaml
deploy/argocd/
  root.yaml                  # the root Application (applied by local-up.sh)
  apps/                      # Helm chart rendering one Application per workload item
    Chart.yaml values.yaml templates/applications.yaml
deploy/local/values/argo-cd.yaml
docs/adr/0009-network-policies-and-pod-security.md
docs/adr/0010-gitops-pull-model.md
docs/day4-tour.md
```

---

### Task 1: Secret scan and public-readiness

**Files:**
- Create: `scripts/secrets-scan.sh`, `.gitleaks.toml` (only if a false positive needs an allowlist entry)
- Modify: `Makefile` (target `secrets-scan`), `deploy/local/versions.env` (`GITLEAKS_IMAGE`)

**Interfaces:**
- Produces:
  - `make secrets-scan` runs gitleaks from its pinned container image over the **full git history** of the repo (`gitleaks git`, with the repo mounted read-only). It exits non-zero on any finding, and prints `SECRETS OK` when clean.
  - Task 4's `ci.yml` reuses the same script.

- [ ] **Step 1:** Write the script and the Make target, then run `make secrets-scan`.
- [ ] **Step 2:** If gitleaks reports a finding, check whether it is real.
  - A **false positive** (for example a test password in `.env.example` or a testcontainers fixture): allowlist that exact path and rule in `.gitleaks.toml`, with a comment saying why.
  - A **real secret**: STOP and report BLOCKED. History rewriting is the user's call.
- [ ] **Step 3:** Check by hand that `.env` is git-ignored and has never been committed (`git log --all -- .env` prints nothing), and that the README states the code was written by agents.
- [ ] **Step 4:** `make secrets-scan` → `SECRETS OK`. Commit `chore: secret scan over git history`.
- [ ] **Step 5 (controller):** **STOP and ask the user** to make the repository public, or to approve the controller running `gh repo edit jmmorell/k8s-order-saga --visibility public --accept-visibility-change-consequences`. Do not continue to Task 5 until the repo is public. Tasks 2–4 do not need it.

---

### Task 2: Pod hardening and Pod Security Admission

**Files:**
- Create: `docs/adr/0009-network-policies-and-pod-security.md` (the Pod Security half; Task 3 adds the network half)
- Modify: `deploy/local/platform/namespaces.yaml` (Pod Security labels, plus the new namespace `argocd`), `deploy/local/platform/{postgres,mailpit}.yaml`, `deploy/local/values/{nats,kube-prometheus-stack}.yaml`
- Modify: `charts/service/templates/{serviceaccount,deployment}.yaml`, `scripts/chart-test.sh`
- Create: `scripts/policy-test.sh` (the Pod Security part)
- Modify: `Makefile` (target `policy-test`), `docs/backlog.md` (remove the Day-4 note about `automountServiceAccountToken` and `platform` not passing `restricted`)

**Interfaces:**
- Produces:
  - **Namespace labels:** `pod-security.kubernetes.io/{enforce,warn,audit}` plus `-version: v1.37`, as in Global Constraints.
  - **Chart:** the ServiceAccount has `automountServiceAccountToken: false`. The pod spec has:
    - `automountServiceAccountToken: false`;
    - a pod-level `securityContext` with `runAsNonRoot: true`, `runAsUser: 65532`, `runAsGroup: 65532` and `seccompProfile: RuntimeDefault`.

    `chart-test.sh` asserts both.
  - **Platform workloads pass `restricted`:**
    - Postgres runs as uid/gid `70`, the alpine image's `postgres` user, with `fsGroup: 70`.
    - Mailpit runs as a non-root uid.
    - Both drop `ALL` capabilities, set `allowPrivilegeEscalation: false` and use `RuntimeDefault` seccomp.
    - NATS gets the same through its chart values, including its reloader and exporter containers.
    - Where a container needs a writable path, give it an `emptyDir`; do not loosen the policy.
  - **node-exporter is disabled** in kube-prometheus-stack. Its hostPath and hostNetwork fail even `baseline`, and on kind it only measures the Docker host. metrics-server still serves `kubectl top`. ADR 0009 records the trade-off.
  - **`make policy-test`** runs `scripts/policy-test.sh`, which in this task:
    1. Asserts each namespace has the expected `enforce` level.
    2. Runs `kubectl run` server-side dry-runs of a privileged pod (`securityContext.privileged: true`) in `shop` and in `platform`, and expects them rejected with a message containing `violates PodSecurity "restricted`.
    3. Runs a server-side dry-run of `kubectl label ns <ns> pod-security.kubernetes.io/enforce=<level> --overwrite` for every namespace, and expects **no** warnings about existing pods.
    4. Prints `POLICY OK`.

- [ ] **Step 1:** Write the ADR 0009 Pod Security section. Cover: `restricted` versus `baseline` per namespace and why, why the observability charts are not pushed to `restricted` this iteration, node-exporter, and ServiceAccount tokens.
- [ ] **Step 2:** Write the policy-test part above and the chart-test assertions. `make chart-test` and `make policy-test` → FAIL.
- [ ] **Step 3:** Implement the labels, the chart changes and the platform security contexts.
- [ ] **Step 4:** Apply to the cluster with `make local-up`, followed by `make local-reload` if pods must restart. Then:
  - `make policy-test` → `POLICY OK`;
  - `make chart-test` → `CHART OK`;
  - every pod in `shop` and `platform` is Ready;
  - `make local-smoke` → `SMOKE OK`, and `make obs-check` → `OBS OK`.
- [ ] **Step 5:** `make local-down && make local-up` from zero, then `make local-smoke` → `SMOKE OK`. Postgres must initialise as uid 70 on a fresh volume. Commit `feat(security): Pod Security restricted, no ServiceAccount tokens`.

---

### Task 3: NetworkPolicies, default-deny

**Files:**
- Create: `deploy/local/policies/{shop,platform,observability,argocd}.yaml`
- Modify: `scripts/policy-test.sh` (the network part), `scripts/local-up.sh` (an `apply_policies` step, called after `install_services`; Task 5 moves this to Argo CD), `docs/adr/0009-network-policies-and-pod-security.md` (the network half), `deploy/local/versions.env` (`CURL_IMAGE` for the probe pods)

**Interfaces:**
- Consumes: the pod labels `app.kubernetes.io/name: <service>` (chart) and the labels of the NATS, Postgres, Mailpit, Collector, Prometheus and Alertmanager pods. Read them from the live cluster; do not guess.
- Produces:
  - **Traffic matrix.** Everything else is denied in `shop` and `platform`. Each "allow" is an ingress rule on the receiver and an egress rule on the sender, whenever the sender's namespace denies egress.

| From | To | Port |
|---|---|---|
| Envoy proxy pods (`envoy-gateway-system`) | `orders` | 8080 |
| `loadgen` | `orders` | 8080 |
| `orders`, `inventory`, `payments` | `postgres` (`platform`) | 5432 |
| `orders`, `inventory`, `payments`, `notifications` | `nats` (`platform`) | 4222 |
| `notifications` | `mailpit` (`platform`) | 1025 |
| all five services | `otel-collector` (`observability`) | 4318 |
| Prometheus (`observability`) | NATS exporter (`platform`) | exporter port as deployed |
| Alertmanager (`observability`) | `mailpit` (`platform`) | 1025 |
| any pod in `shop` / `platform` | kube-dns (`kube-system`) | 53 TCP+UDP |
| within `observability`, within `argocd` | same namespace | any |
| Prometheus | `otel-collector` | 8889 |

  - Policies select peers with `namespaceSelector` on `kubernetes.io/metadata.name` together with a `podSelector`, never by IP.
  - **`scripts/policy-test.sh` gains network probes.** Each probe is a short-lived pod from `$CURL_IMAGE`, built to pass `restricted` (non-root, no capabilities, seccomp), that runs `curl --max-time 3` and is deleted afterwards. Each is created with labels that impersonate a sender, or none:
    - **Allowed**:
      - pod labelled `app.kubernetes.io/name: loadgen` in `shop` → `http://orders.shop:8080/healthz`;
      - the same pod labelled `notifications` → `telnet://mailpit.platform:1025`.
    - **Denied** (must time out):
      - unlabelled pod in `shop` → `orders:8080`;
      - unlabelled pod in `shop` → `postgres.platform:5432`;
      - `loadgen`-labelled pod → `postgres.platform:5432`;
      - pod in `platform` → `orders.shop:8080`;
      - `notifications`-labelled pod → `https://example.com` (no internet egress from `shop`).
    - Prints `POLICY OK` only if every probe matches.

- [ ] **Step 1:** Confirm that kindnet enforces NetworkPolicy on this kind version. Apply a temporary deny-all to a throwaway namespace and show that a probe times out. Record the result in ADR 0009. If it is **not** enforced, STOP and report BLOCKED.
- [ ] **Step 2:** Add the network probes to `policy-test.sh` → `make policy-test` FAILS, because the "denied" probes succeed.
- [ ] **Step 3:** Write the four policy files and `apply_policies`, then apply them.
- [ ] **Step 4 (Review Focus 1):**
  - `make policy-test` → `POLICY OK`.
  - Every pod stays Ready, with no restarts caused by probes.
  - `make local-smoke` → `SMOKE OK`; `make obs-check` → `OBS OK`; `scripts/trace-check.sh <id>` → OK.
  - Loki still receives logs.
  - The payment-outage alert still reaches Mailpit. A rule test is not enough here: briefly set `PAYMENT_FAILURE_RATE=1`, wait for the email, then restore it.
  - Record in ADR 0009 whether probes and port-forwards bypass the policies on kindnet, and that on GKE (Dataplane V2) they must be re-verified.
- [ ] **Step 5:** `make local-up` again → idempotent. Commit `feat(security): default-deny NetworkPolicies with explicit allows`.

---

### Task 4: CI workflows and the image-tag bump

**Files:**
- Create: `.github/workflows/ci.yml`, `.github/workflows/release.yml`
- Create: `scripts/bump-image-tag.sh`, `scripts/bump-image-tag_test.sh`
- Modify: `Dockerfile` (cross-compile for buildx), `deploy/local/values/{orders,inventory,payments,notifications,loadgen}.yaml` (`image.repository: ghcr.io/jmmorell/k8s-order-saga/<service>`, `tag: dev`), `scripts/local-up.sh` (`build_and_load_images` builds and loads `<repository>:<tag>` read from each values file), `Makefile` (target `ci-local`)

**Interfaces:**
- Produces:
  - **`Dockerfile`:** the build stage uses `FROM --platform=$BUILDPLATFORM golang:1.27` and builds with `GOOS=$TARGETOS GOARCH=$TARGETARCH`, so a multi-arch build cross-compiles instead of emulating. A local `docker build` behaves as before.
  - **`ci.yml`:**
    - Triggers: `push` (all branches) and `pull_request`.
    - Jobs: `lint` (golangci-lint action, pinned); `test` (`go test -race ./...`; testcontainers works with the runner's Docker); `charts` (installs pinned `helm` and `kubeconform`, runs `make chart-test` and `make rules-test`); `secrets` (`scripts/secrets-scan.sh`); `bump-script` (`scripts/bump-image-tag_test.sh`).
    - Permissions: `contents: read`.
  - **`release.yml`:**
    - **On `pull_request`:** a matrix over the five services. For each it builds `linux/amd64,linux/arm64` with buildx, without pushing. It then builds a scan copy for the runner's own architecture with `load: true`, and runs Trivy on it (`--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`).
    - **On `workflow_dispatch`:**
      1. The same build and scan.
      2. Push both arches to `ghcr.io/jmmorell/k8s-order-saga/<service>:<short-sha>`, plus `:latest-main` as a convenience tag. Deployment never reads `:latest-main`.
      3. Then one `bump` job (`needs:` all five), which runs `scripts/bump-image-tag.sh <short-sha>` on a fresh checkout of `main`.
      4. The bump job commits `chore(deploy): image tag <short-sha> [skip ci]` as `github-actions[bot]` and pushes to `main`.
      5. If the push is rejected because `main` moved, it rebases once and retries; a second failure fails the job (Review Focus 4).
    - Permissions: `contents: read` by default; `packages: write` for the push jobs; `contents: write` only for the `bump` job.
    - `concurrency: release` with `cancel-in-progress: false`, so two dispatches queue rather than race.
  - **`scripts/bump-image-tag.sh <tag>`:**
    - Rewrites only the `tag:` line under `image:` in each of the five values files.
    - Fails with a clear message if the tag is empty or not `[0-9a-f]{7,40}|dev`, or if any file lacks that line.
    - Leaves every other byte untouched, and is idempotent.
  - **`scripts/bump-image-tag_test.sh`:**
    - Copies the five values files to a temp directory, runs the script, and asserts with `diff` that exactly one line changed per file.
    - Asserts a second run changes nothing.
    - Asserts a bad tag fails without modifying any file.
    - Prints `BUMP OK`.
  - **`make ci-local`:** runs `actionlint` from its pinned container over `.github/workflows`, then the bump test.

- [ ] **Step 1:** Write `bump-image-tag_test.sh` → FAIL (no script). Write the script → `BUMP OK`.
- [ ] **Step 2:** Write both workflows and the Dockerfile change. Update the values files and `build_and_load_images`. `make ci-local` → actionlint reports 0 issues and prints `BUMP OK`.
- [ ] **Step 3:** `docker buildx build --platform linux/amd64,linux/arm64 --build-arg SERVICE=orders .` succeeds locally. `make local-up && make local-smoke` → `SMOKE OK`, with the pods running `ghcr.io/jmmorell/k8s-order-saga/<service>:dev` (`kubectl get pods -o jsonpath` confirms the image).
- [ ] **Step 4:** Commit `feat(ci): CI checks, release workflow and image-tag bump`. **Push the branch** (`git push -u origin day4/gitops-security`; approving this plan approves pushing this branch). Confirm `ci.yml` runs green on GitHub (`gh run list --branch day4/gitops-security`, then `gh run watch <id>`). `release.yml` cannot be dispatched until it is on `main`; its `pull_request` path runs once the PR is opened. Note this in the report.

---

### Task 5: Argo CD and the app-of-apps

**Files:**
- Create: `docs/adr/0010-gitops-pull-model.md`
- Create: `deploy/local/values/argo-cd.yaml`, `deploy/argocd/root.yaml`, `deploy/argocd/apps/{Chart.yaml,values.yaml,templates/applications.yaml}`
- Modify: `deploy/local/versions.env` (`ARGOCD_CHART_VERSION`), `scripts/local-up.sh`, `scripts/chart-test.sh` (lint and render `deploy/argocd/apps`), `Makefile` (target `argocd`; `local-reload` keeps working)

**Interfaces:**
- Consumes: the public repo (Task 1 Step 5), the GHCR image references and `build_and_load_images` (Task 4), and the policy files (Task 3).
- Produces:
  - **Argo CD install.** The Argo CD chart is pinned, in namespace `argocd`, from `deploy/local/values/argo-cd.yaml`.
    - The server Service stays ClusterIP.
    - Dex, notifications and ApplicationSet controllers are disabled, since they are unused.
    - Every container has requests and limits.
    - `make argocd` port-forwards the UI to `https://localhost:8443` and prints the command that reads the initial admin password. It does not print the password.
  - **`deploy/argocd/apps` chart** (values: `repoURL: https://github.com/jmmorell/k8s-order-saga.git`, `revision: main`). It renders one `Application` per item:
    - `svc-<name>` for each of the five services: chart `charts/service`, `valueFiles: [../../deploy/local/values/<name>.yaml]`, release name `<name>`, namespace `shop`;
    - `saga-observability`: chart `charts/saga-observability`, namespace `observability`;
    - `policies`: directory `deploy/local/policies`.

    Every Application uses `syncPolicy.automated` with `prune: true` and `selfHeal: true`, and `revision` as its `targetRevision`. They do not create namespaces; `local-up.sh` owns those.
  - **`deploy/argocd/root.yaml`:** Application `root` in `argocd`, path `deploy/argocd/apps`, automated sync. `local-up.sh` applies it with `revision` overridden by `ARGOCD_REVISION`, default `main`, so a branch can be tested before merge.
  - **`local-up.sh`:**
    - `install_services` and `apply_policies` are replaced by `install_argocd`, then `apply_root_app`, then `wait_apps` (every Application `Synced` and `Healthy`, 300 s timeout).
    - `build_and_load_images` still runs first, so the `dev` tag is loaded before Argo CD syncs.
    - **Migration:** if a Helm release of a service from day 2 or 3 is present, the script tells the user to run `make local-down && make local-up` once, and exits non-zero. It does not try to adopt the old releases.
  - **`make local-reload`** keeps working: it rebuilds, loads and runs `rollout restart`. A restart annotation is a change Argo CD's self-heal ignores, so pods are not reverted. Confirm this live.

- [ ] **Step 1:** Write ADR 0010. Cover:
  - push versus pull, and why CI never gets cluster credentials;
  - the manual trigger;
  - the bootstrap-versus-workload split and why platform charts stay in the script this iteration (CRD ordering), with moving them into Argo CD as roadmap;
  - the public repo and registry;
  - what changes on GKE.
- [ ] **Step 2:** Extend `chart-test.sh` to lint and render `deploy/argocd/apps` and to assert seven Applications with the expected names and paths → FAIL. Write the chart → `CHART OK`.
- [ ] **Step 3:** Implement the install and the `local-up.sh` changes. Push the branch.
- [ ] **Step 4:** `make local-down && ARGOCD_REVISION=day4/gitops-security make local-up` from zero → exit 0.
  - All Applications are Synced and Healthy.
  - `make local-smoke` → `SMOKE OK`, `make obs-check` → `OBS OK`, `make policy-test` → `POLICY OK`.
- [ ] **Step 5 (Review Focus 3):** `kubectl --context kind-order-saga -n shop scale deploy/orders --replicas=1` → within 3 minutes Argo CD restores 2 replicas. Record the time.
- [ ] **Step 6 (Review Focus 2):**
  1. Commit a values change on the branch that sets `payments` `image.tag` to a tag that does not exist (`0000000`), and push it.
  2. Argo CD syncs, and the new ReplicaSet's pod reaches `ErrImagePull`.
  3. The old payments pod keeps serving: `make local-smoke` → `SMOKE OK`. The Application shows `Degraded` or `Progressing`.
  4. `git revert` that commit and push. Argo CD returns to Healthy.

  Record the timeline: it is the seed of the day-6 "bad rollout" drill.
- [ ] **Step 7:** `ARGOCD_REVISION=day4/gitops-security make local-up` again → idempotent. `make chart-test && make lint` → OK. Commit `feat(gitops): Argo CD app-of-apps for the workload`.

---

### Task 6: Guided tour, README and checkpoint

**Files:**
- Create: `docs/day4-tour.md`
- Modify: `README.md`, `docs/backlog.md`

The author owns this day ("Author + agents" in the spec); the tour is how they take over what agents built. It follows the format of `docs/day3-tour.md`: every command passes `--context kind-order-saga`, has its expected output in one line, and every exercise has a restore step.

- [ ] **Step 1:** Write `docs/day4-tour.md`:
  1. Pod Security: the labels, what `restricted` forbids, and a rejected privileged pod (dry-run).
  2. ServiceAccounts without tokens: `kubectl exec` shows `/var/run/secrets/kubernetes.io` is absent, and why that matters.
  3. NetworkPolicies: the traffic matrix as a picture. Read one policy file, run `make policy-test`, and do one "break it" exercise: remove one allow, watch the saga stall and the alert fire, then let Argo CD self-heal it back.
  4. Argo CD: `make argocd`, the root app and its children, sync, self-heal (Review Focus 3), and prune.
  5. The release path:
     - CI checks on push;
     - `release.yml` on a PR (build and scan only);
     - a dispatch → GHCR → tag commit on `main` → Argo CD rollout.
     - Include the exact `gh workflow run release.yml --ref main` command, and how to watch it (`gh run watch`, then the Argo CD UI).
  6. A bad rollout (Review Focus 2) and rollback with `git revert`.
  7. Trivy: read one scan result, and what "fixable HIGH/CRITICAL" means.
  8. What is still missing (deferred to iteration 2): image signing, Secret Manager, private networking, platform charts in Argo CD, and NetworkPolicies on GKE.
- [ ] **Step 2:** Run every command in the tour against the cluster, fix what does not match, and undo every exercise. Section 5's dispatch can only run after merge; mark it "after the PR is merged" and verify only its syntax.
- [ ] **Step 3:** README:
  - Add a "Security" section and a "Deploy from Git" section.
  - Add ADRs 0009 and 0010 to the index.
  - Add a **"Day 4 checkpoint"**:
    - [ ] `make local-down && make local-up && make local-smoke && make policy-test` → `SMOKE OK`, `POLICY OK`
    - [ ] Work through `docs/day4-tour.md`
    - [ ] After merging: run `release.yml` once, see the tag commit on `main`, and see Argo CD roll it out
    - [ ] Make the GHCR packages public if the first pull fails (Settings → Packages)
    - [ ] Explain in own words: `baseline` vs `restricted`, default-deny and why DNS needs an allow, push vs pull deploys, why CI has no cluster credentials, what self-heal does
    - [ ] Read ADRs 0009–0010 and be able to defend them
- [ ] **Step 4:** `docs/backlog.md`: add a "Day 4" section with the deferred findings, and remove the day-2 "Day-4 notes" entry, which is resolved.
- [ ] **Step 5:** `make local-smoke && make obs-check && make policy-test` → OK. Commit `docs: day-4 tour, README and checkpoint`. Push.

---

## Next

Day 5 (GKE and Cloud SQL with Terraform) gets its own plan. It reuses `deploy/argocd/apps` with GKE values. It must re-verify the NetworkPolicies on Dataplane V2, and replace the in-cluster Postgres allow with the Cloud SQL Auth Proxy sidecar's egress.
