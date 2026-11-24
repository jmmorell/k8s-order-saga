# ADR 0007: Gateway API with Envoy Gateway as the entry point

## Status

Accepted

## Context

The cluster needs one way in from the host: `http://localhost:8080` must reach the `orders` service, and nothing else may be reachable. The choice is the API that describes the route and the controller that implements it.

## Decision

- Use the Gateway API, not Ingress. Gateway API is the successor to Ingress: it separates the infrastructure (GatewayClass, Gateway) from the routing rules (HTTPRoute), has typed matching instead of controller-specific annotations, and is what GKE implements natively.
- Use Envoy Gateway, installed from `oci://docker.io/envoyproxy/gateway-helm`, pinned to 1.10.0 (the latest stable release, whose compatibility matrix lists Kubernetes 1.34 to 1.37) in `deploy/local/versions.env`. It is a small, conformant implementation. The `gateway-helm` chart installs the Gateway API CRDs as well as Envoy Gateway's own; on GKE the Gateway API CRDs come with the cluster. On a fresh cluster the Envoy proxy Deployment rolls once shortly after creation, when the controller applies the resource patch. ingress-nginx is not used.
- Resources in `deploy/local/gateway/`: GatewayClass `eg`, Gateway `shop` (HTTP, port 80, namespace `shop`) and HTTPRoute `orders`, which sends only the `/orders` path prefix to Service `orders:8080`. Any other path, including `/healthz`, gets a 404 from the proxy.
- The GatewayClass points to an `EnvoyProxy` resource. It sets the proxy Service to `type: NodePort`, with a strategic-merge patch that pins the port 80 entry to `nodePort: 30080`, and `externalTrafficPolicy: Cluster`. The default `Local` would drop traffic whenever the proxy pod is not on the control-plane node, which is the one node that maps 30080 to the host. The same resource sets requests and limits for the proxy container.
- A fixed NodePort instead of a LoadBalancer: kind has no cloud load balancer, so a LoadBalancer Service would stay `Pending` forever unless we add MetalLB or cloud-provider-kind. A fixed NodePort plus the kind port mapping in `kind-cluster.yaml` needs nothing else. The Envoy proxy Service is the only NodePort in the cluster.

## Alternatives considered

- Ingress with ingress-nginx: familiar, but Ingress is frozen, depends on annotations, and teaches the older model.
- Other Gateway API controllers (Cilium, Istio, NGINX Gateway Fabric): either need a larger platform or have less documentation for a first read.
- `kubectl port-forward` to `orders`: no extra component, but it is not a route, and teaches nothing about the entry point.

## Consequences

- The route, the listener and the proxy settings are all declarative and reviewable.
- On GKE the `EnvoyProxy` resource is dropped (or set to `LoadBalancer`) and the GatewayClass can switch to GKE's own controller. The Gateway and HTTPRoute stay the same, apart from `gatewayClassName`.
- Envoy Gateway creates the proxy Deployment and Service itself in `envoy-gateway-system`, with generated names. Tests should find them through labels, not names.
