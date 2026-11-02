# 0008. Traefik ingress, subdomains, tunnels as a capability

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Services need routing and TLS. Many homelab servers sit behind NAT. The mobile app needs a reachable server with valid TLS.

## Decision

- **Traefik** is the ingress in both Compose and k3s (k3s ships it by default). **Routes come from the core** (Traefik HTTP provider; IngressRoute objects in Kubernetes), and Traefik gets **no Docker socket**.
- **Subdomain per service** (`git.acme.com`, `files.acme.com`), with TLS via Let's Encrypt. Wildcard certificates are optional and need a DNS provider token.
- **Trying Orglane needs no domain:** `--demo` runs on localhost.
- **Production self-hosting requires a domain.** The setup wizard shows the DNS records to create and checks propagation and reachability.
- **Reachability is the `ingress.tunnel` capability**, offered in the wizard: "I have a public IP" / **Cloudflare Tunnel** (paste a token; Orglane runs `cloudflared` itself) / **Orglane Link**.
- There is no heavy manual networking documentation (for example VPS + WireGuard).
- **Customer subdomains** (Link tunnels, cloud instances) live on a **separate registrable domain**, never under `orglane.app`, so customer instances can't share cookies with the project's own sites (landing, dashboard, billing). The domain will be acquired together with Link.

## Consequences

- One routing model across self-hosted and cloud.
- Link becomes the zero-configuration path and a natural paid offering, but is never required.
