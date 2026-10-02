# Deploying jmap-bridge

This covers the two things that are not in the main README: getting the
project onto GitHub and publishing the container image to GHCR, then running
it on Kubernetes. For the Gmail consent setup and the reverse-proxy examples,
see the main [README §Deployment](../README.md#deployment).

Layout:

```
Dockerfile                     static, non-root, distroless (FR-D.1)
.dockerignore                  keeps secrets/data out of the build context
.github/workflows/release.yml  tag → multi-arch image on GHCR (packaging only)
deploy/k8s/                    kustomize stack (Deployment, Service, Ingress,
                               PVC, ConfigMap; Secret is created out of band)
```

## 1. Create the GitHub repository

The module path is already `github.com/CaffeinatedTech/jmap-bridge`, so the
remote must match or the image label and imports drift from the repo.

```sh
# from the repo root, with the git repo already initialised
gh repo create CaffeinatedTech/jmap-bridge --public --source=. --remote=origin --push
```

Or by hand:

```sh
git remote add origin git@github.com:CaffeinatedTech/jmap-bridge.git
git push -u origin main
```

`--public` is a choice: a **private** repo makes the GHCR package private by
default, and Kubernetes then needs an `imagePullSecret` (step 3). For a
self-hosted mail bridge, public source + a private or public image both work.

**No CI, by decision (D-10).** The workflows here only publish images; the
build/vet/gofumpt/lint/`go test -race` gates are run locally before every
commit and are the release bar (see `AGENTS.md`).

## 2. Publish the image to GHCR

Images are named `ghcr.io/<owner>/jmap-bridge` (GHCR lower-cases the owner).
For this repo: `ghcr.io/caffeinatedtech/jmap-bridge`.

### The tag-driven way (normal releases)

`.github/workflows/release.yml` builds `linux/amd64` and `linux/arm64` and
pushes on every `v*` tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

It runs no tests on purpose — tag only after the local gates are green. The
resulting tags are the semver tags plus `latest`. No repository settings are
required: the workflow requests `packages: write` explicitly on the
`GITHUB_TOKEN`.

After the first successful run, open **GitHub → Packages → jmap-bridge →
Package settings** and:

- **Connect repository** to `CaffeinatedTech/jmap-bridge` so the package
  inherits the repo's permissions and its README.
- set visibility to **Public** if you want `docker pull` without a login
  (private is the default and fine; just add `imagePullSecret` in k8s).

### The manual way (first image, or no Actions)

```sh
# A Personal Access Token with write:packages (classic) or the GHCR scope.
echo "$CR_PAT" | docker login ghcr.io -u <github-user> --password-stdin

docker buildx create --name jmap-bridge --use
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t ghcr.io/caffeinatedtech/jmap-bridge:v0.1.0 \
  -t ghcr.io/caffeinatedtech/jmap-bridge:latest \
  --build-arg VERSION=v0.1.0 \
  --build-arg REVISION="$(git rev-parse --short HEAD)" \
  --push .
```

Then smoke-test the image (any machine with a Docker daemon):

```sh
docker run --rm ghcr.io/caffeinatedtech/jmap-bridge:v0.1.0 --version
```

> Docker is not a required local gate for this project: the dev machine runs
> the native binary. The image is verified on a docker-capable host before an
> M7 release, per `AGENTS.md`.

### Pin what you deploy

`latest` is convenient, not safe. In production pin a tag or, better, a
digest: `ghcr.io/caffeinatedtech/jmap-bridge:v0.1.0@sha256:…`.

## 3. Run it on Kubernetes

Prerequisites: a cluster, a default `StorageClass` for the PVC, and an Ingress
controller (the example targets Traefik, the k3s default). TLS terminates at the
Ingress (FR-D.2, D-12).

### a. Configure

Edit `deploy/k8s/configmap.yaml`: set `base_url` and the account blocks (the
sample ships a `personal` password account and a commented Gmail block). The
host in `ingress.yaml` must match `base_url`'s host, and both must match the
redirect URI on your Google OAuth client (FR-D.3, FR-D.11).

Set the image in `deployment.yaml` (or bump it later with
`kustomize edit set image`, see the commented block in `kustomization.yaml`).

### b. Create the Secret (never commit it)

Every secret key is `JMAP_BRIDGE_<ACCOUNT>_<FIELD>` (FR-A.2), matching the
account ids in the ConfigMap. `deploy/k8s/secret.example.yaml` lists the
shape.

```sh
kubectl create namespace jmap-bridge
kubectl -n jmap-bridge create secret generic jmap-bridge-secrets \
  --from-literal=JMAP_BRIDGE_SECRET_KEY="$(openssl rand -base64 32)" \
  --from-literal=JMAP_BRIDGE_PERSONAL_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=JMAP_BRIDGE_PERSONAL_IMAP_PASSWORD='…'
  # add JMAP_BRIDGE_GMAIL_OAUTH2_CLIENT_SECRET=… for a Gmail account
```

`JMAP_BRIDGE_SECRET_KEY` is what keeps OAuth tokens encrypted at rest
(FR-A.8); without it the bridge warns loudly and stores them in plaintext.

### c. TLS certificate

The example uses a namespaced cert-manager `Issuer` (`issuer.yaml`,
Let's Encrypt via Cloudflare DNS-01). Copy the token secret into the
`jmap-bridge` namespace first, then apply; cert-manager issues
`jmap-bridge-tls` from the Ingress annotation:

```sh
kubectl -n default get secret cloudflare-api-token-secret -o json \
  | sed 's/"namespace": "default"/"namespace": "jmap-bridge"/' \
  | kubectl apply -f -
```

Or skip `issuer.yaml` and create `jmap-bridge-tls` yourself:

```sh
kubectl -n jmap-bridge create secret tls jmap-bridge-tls \
  --cert=fullchain.pem --key=privkey.pem
```

### d. Apply

```sh
kubectl apply -k deploy/k8s
kubectl -n jmap-bridge rollout status deploy/jmap-bridge
```

The manifest is deliberately a **single replica** with `strategy: Recreate`:
the cache is one SQLite writer on a ReadWriteOnce volume (PLAN §10). Do not
scale it horizontally.

### e. First consent (Gmail / any OAuth account)

An OAuth account has no credentials until someone consents, so its engine
cannot complete a sync pass and `/readyz` stays `503`. The Service sets
`publishNotReadyAddresses: true` so this is not a deadlock: the Ingress still
reaches the pod, and the `/oauth/…` endpoints are never behind the client
token. Open once per OAuth account:

```
https://jmap.example.com/oauth/gmail/start
```

Approve; the bridge stores the refresh token (encrypted when
`JMAP_BRIDGE_SECRET_KEY` is set), wakes the account, and `/readyz` flips to
`ready` once every account has synced. The same URL re-consents after a
revoked or expired token (for example Google's 7-day Testing expiry). Do not
patch the readiness probe to bootstrap consent — that is exactly what the
Service setting exists for.

### f. Dev / tunnel mode

No public hostname yet? Forward the Service and use a loopback `base_url`
(cleartext is allowed only on loopback, FR-A.3):

```sh
kubectl -n jmap-bridge port-forward svc/jmap-bridge 8080:80
# then point the client at http://127.0.0.1:8080/{account}
```

For a Gmail account over the tunnel, use a **Desktop app** OAuth client whose
redirect URI is `http://127.0.0.1:8080/oauth/{account}/callback` and set
`base_url = "http://127.0.0.1:8080"` while you consent.

## 4. Verify

```sh
kubectl -n jmap-bridge get pods
kubectl -n jmap-bridge logs deploy/jmap-bridge | tail

curl -sS https://jmap.example.com/healthz          # "ok"
curl -sS https://jmap.example.com/readyz           # "ready" once every account has synced
```

`/healthz` is process liveness; `/readyz` is false until every account with an
IMAP backend has completed a sync pass, and false again if an account hits an
authentication failure (a dead Gmail refresh token, for example) — re-consent
via `/oauth/{account}/start` (FR-D.4). Until then `get pods` shows `0/1`; that
is expected, and the consent URL stays reachable (see e) because the Service
publishes not-ready addresses.

For a Gmail account, finish onboarding from a browser:

```
https://jmap.example.com/oauth/{account}/start
```

Back up by copying the PVC while the pod is stopped (FR-D.9).
