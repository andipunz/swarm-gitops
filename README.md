# swarm-gitops

Pull-based GitOps for Docker Swarm. Developers only need GitHub: they put a
`.swarm/` folder into their repo, push, and see the result as a check on the
commit and under *Environments* in the repo. The controller runs on a Swarm
manager and only ever makes outbound HTTPS requests.

```
                         outbound only
┌──────────────────────────┐        ┌───────────────────────────────┐
│ swarm-gitops (manager)   │───────▶│ GitHub API (GitHub App)       │
│  scan org every minute   │◀───────│ repos, .swarm/ files, branches│
│  render → policy → deploy│───────▶│ check runs, deployments       │
│  check image digests     │───────▶│ ghcr.io (digest lookups)      │
└────────────┬─────────────┘        └───────────────────────────────┘
             │ docker stack config / deploy / rm
             ▼
        Docker Swarm
```

Each scan:

1. **Discover**: one GraphQL query lists all repos with their
   `.swarm/deploy.yml`; for repos that have one, all branches with head commit
   and `.swarm/` tree.
2. **Plan**: environments × matching branches → stacks (`<repo>-<env>`, or
   `<repo>-<env>-<branch>` for branch patterns). Only environments an admin
   enabled count.
3. **Render**: `docker stack config` merges the env's compose files and
   interpolates variables, using exactly the same code as `docker stack deploy`.
4. **Policy**: checks the rendered stack (no bind mounts, no foreign images,
   no published ports, secrets never from git, …).
5. **Deploy**: only if the rendered stack changed (or on an empty commit),
   and only once the commit's own CI has finished (see [Waiting for CI](#waiting-for-ci)),
   then wait for the rollout and report back.
6. **Remove**: stacks whose source is *provably* gone. See [Removal](#removal-safety).

In between, every 2 minutes, running services are checked for **new image
digests** behind their tags (`:main`, `:latest`, …) and rolled when one appears.

---

## For developers

### Repo layout

```
.swarm/
├── deploy.yml           # environments and their branches
├── stack.yml            # base stack (compose v3, like docker stack deploy)
├── stack.prod.yml       # overrides per environment
├── stack.staging.yml
├── prod.env             # non-secret variables per environment
├── staging.env
└── config/…             # files used as Swarm configs
```

See [`examples/einsatz-app`](examples/einsatz-app) for a complete example.

### `deploy.yml`

```yaml
environments:
  prod:
    ref: main                          # branch that deploys to this env
    files: [stack.yml, stack.prod.yml] # merged in order (default: stack.yml + stack.<env>.yml if present)
    vars: prod.env                     # optional variables file
    url: https://einsatz.example.org   # shown in GitHub
    bind_owner: "1000:1000"            # optional owner of bind folders the controller creates
  staging:
    ref: develop
  sandbox:
    ref: "sandbox/*"                   # pattern: one stack per matching branch
```

- Environment names: lowercase letters, digits, dashes.
- An environment only deploys once an org admin has enabled it for the repo
  (custom property `swarm-environments`). Otherwise the commit gets a neutral
  check "Environment prod is not enabled".
- `prod` only deploys from a **protected** branch.
- `deploy.yml` is always read from the **default branch**. A feature branch
  can't give itself a new environment.

### Variables

Compose variables come from the env's `vars` file plus these built-ins:

| Variable | Value |
|---|---|
| `${ENV}` | environment name, e.g. `prod` |
| `${STACK}` | stack name, e.g. `einsatz-app-prod` |
| `${GIT_SHA}` | deployed commit |
| `${GIT_REF}` | branch |
| `${REPO}` | repository name |
| `${STACK_DATA}` | the stack's own bind folder, e.g. `/srv/swarm/einsatz-app/prod` |

Compose never interpolates label **keys**. The controller replaces `${STACK}`
and `${ENV}` in label keys, so Traefik router names stay unique per stack:
`` traefik.http.routers.${STACK}.rule: Host(`${DOMAIN}`) ``.

### Feedback

- **Check run `swarm / <env>`** on the commit: ✅ deployed, ❌ policy
  violation / invalid stack / failed rollout (with service table, task errors
  and the last log lines), ⚪ no changes / not enabled.
- **Environments** sidebar and deployment history in the repo, with the
  environment URL.
- **Validate before merge:** copy
  [`examples/einsatz-app/.github/workflows/swarm-check.yml`](examples/einsatz-app/.github/workflows/swarm-check.yml)
  into your repo. It runs the same rendering and policy on every PR.

### Redeploy

```sh
git commit --allow-empty -m "redeploy" && git push
```

An empty commit (same content as its parent) forces a rolling restart of the
stack, even if nothing changed. Normal commits that don't change the rendered
stack (README edits, code outside `.swarm/`) don't restart anything.

### New images

If `stack.yml` uses a tag (`ghcr.io/example/app:main`), a new image
pushed to that tag is rolled out automatically within ~2 minutes. It shows up
as a deployment "New image: app → sha256:…" in the repo. To opt out, pin
by digest (`app:1.4.2@sha256:…`); pinned images are never touched.

Use `update_config.failure_action: rollback`, so a broken image rolls back
automatically and is reported as failed.

### Waiting for CI

A commit is deployed only once its own CI has finished: every check run on it
except swarm-gitops' own (the workflow building the image, tests, the
swarm-check, …) has to be completed. Otherwise a commit that changes `.swarm/`
together with the code would be rolled out with the *previous* commit's image
under the same tag (`:develop`, `:main`), because its own image isn't built
yet - and fail as soon as the new configuration needs the new code (a new
health endpoint, a new setting).

- The `swarm / <env>` check run appears once the waiting is over.
- A commit without any check runs is deployed one scan later (time for
  GitHub to create them).
- CI that finished unsuccessfully doesn't block the deploy; the check run
  says which checks failed.
- After `CI_WAIT_TIMEOUT` (default 30 minutes) the commit is deployed
  anyway, with a note in the check run. `CI_WAIT_TIMEOUT=0` turns waiting off.
- GitHub App auth only: under token auth the Checks API isn't available, so
  commits deploy right away as before.

### Bind mounts

Each stack has its own folder on the nodes, `${STACK_DATA}`
(`/srv/swarm/<repo>/<env>` by default). Mount anything below it:

```yaml
    volumes:
      - ${STACK_DATA}/uploads:/app/uploads
      - ${STACK_DATA}/postgres:/var/lib/postgresql/data
```

- Missing folders are **created automatically** on every node where the
  service may run, before the deploy (owner from `bind_owner`, else root).
- Folders outside the allowed ones (policy `bind_mounts`) are refused, as are
  other stacks' folders.
- A rule can also be scoped to specific compose service names (`services:`),
  not just repos - useful for something like a metrics exporter that
  legitimately needs a read-only view of the whole host (`path: /`): scoping
  it to `repos` alone would let every other service the repo's compose files
  define (present or future) claim the same access. `path: /` is rejected at
  load time unless `services` is also set, for exactly this reason. The
  per-node prep job itself only ever gets read-only access to such a prefix
  too (nothing under it is ever auto-created), so even the job's own
  short-lived container can't write to it.
- Two rules stop symlink tricks. Docker follows symlinks when it mounts a
  folder, so a container could otherwise reach the host:
  - **No nesting:** a writable mount may not contain another mount's folder.
  - **No subfolders of former writable mounts:** once `${STACK_DATA}` itself
    was mounted writable, mounting `${STACK_DATA}/x` later is refused, because
    the container could have replaced `x` with a symlink. If this blocks you,
    ask an admin (`reset-binds`).
  - Symlinks in a folder's path are always refused on the node.
- Relative bind mounts (`./data:/data`) are not supported; use `${STACK_DATA}/data`.

### Hostnames (Traefik)

```yaml
    deploy:
      labels:
        traefik.enable: "true"
        traefik.http.routers.${STACK}.rule: Host(`app.example.com`)
        traefik.http.routers.${STACK}.entrypoints: websecure
        traefik.http.services.${STACK}.loadbalancer.server.port: "8080"
```

- **A hostname belongs to the stack that routes it first.** Every other stack
  gets a failed check "hostname … is already used by stack …". This includes
  stacks not managed by swarm-gitops (Portainer, infrastructure).
- Hostnames must fit the environment, e.g. prod `*.example.com`,
  sandbox `*.sandbox.example.com`. Admins can reserve hostnames for a
  repository.
- Every router needs a rule with `Host()` (`HostSNI()` for TCP). `||`, `!`,
  `HostRegexp` and rules without a host are refused, because they could catch
  other stacks' traffic. Combine further matchers with `&&`, e.g.
  `` Host(`a`) && PathPrefix(`/api`) ``.
- Router, service and middleware names must start with the stack name. Use
  `${STACK}` or `${STACK}-api`. On top of that, each name belongs to the
  stack that declares it first (like hostnames): this catches the case where
  one stack's name is itself a prefix of another's (e.g. `app-prod` and
  `app-prod-x`), which the prefix rule alone wouldn't stop from colliding.
- Routers may only point to the stack's own services (no `api@internal`).
- Traefik labels belong under `deploy.labels`, not container `labels`.

### Secrets

Secrets are never in git (`file:`/`environment:` are always rejected). Two
ways to provide one, both opt-in via the policy:

**Plain Swarm secrets** (`allow_external_secrets: true`, on by default in
[`deploy/policy.yml`](deploy/policy.yml)) — create them ahead of time, no
plugin needed:

```sh
docker secret create einsatz-app-prod_db_password -
```
```yaml
secrets:
  db_password:
    external: true
    name: ${STACK}_db_password
```

These aren't scoped by swarm-gitops: name them so one repo can't guess
another's (e.g. prefix with `${STACK}`), same as you would on any plain Swarm.

**Your own secrets driver plugin** (`secret_driver: <name>` in the policy) —
for per-repo/per-env scoping using the labels swarm-gitops sets, see
[Secrets plugin](#8-secrets-plugin-optional) below:

```yaml
secrets:
  db_password:
    driver: your-org/secrets
```

### Removal

A stack is removed when its source is gone:

- the environment is deleted from `deploy.yml`, or its compose files are deleted
- the branch is deleted (typical for `sandbox/*`)
- `.swarm/deploy.yml` is deleted
- the repository is archived
- an admin disables the environment

Named volumes are **never** removed automatically.

### Policy (what is not allowed)

| Rule | Why |
|---|---|
| Bind mounts outside the stack's folder (incl. `docker.sock`), nested or symlinked binds, `driver_opts` bind tricks | host takeover |
| Hostnames or Traefik router/service/middleware names of other stacks, outside the env's domains, catch-all rules | stealing traffic |
| `privileged`, `cap_add`, `network_mode`, `pid`, `ipc`, `devices`, `security_opt` | host takeover |
| Published `ports`, unless explicitly listed in `published_ports` | bypasses Traefik, port conflicts |
| Images outside `ghcr.io/example/` and official images | supply chain |
| External networks other than `traefik-public` | isolation between stacks |
| External/custom-named volumes and configs | access to other stacks' data |
| `secrets:` using `file:`/`environment:`, or a method the policy doesn't enable | secrets in git |
| `env_file`/`file` paths outside `.swarm/` | reading controller files |
| Labels starting with `swarm-gitops.` | reserved for the controller |

The policy lives in [`deploy/policy.yml`](deploy/policy.yml).

---

## For admins

### 1. Authenticate to GitHub

Either works; the controller picks whichever is configured (see
[Configuration](#configuration)). Both need the same permissions:
repository Contents *read*, Metadata *read*, Checks *read & write*,
Deployments *read & write*, Administration *read*, and organization Custom
properties *read*.

Administration *read* is needed to check **what kind** of branch protection a
branch has (`GET .../branches/:branch/protection`), not just whether it has
any: `REQUIRE_PROTECTED` only accepts protection that actually requires a
pull request (classic protection with required reviews, or a ruleset with a
`pull_request` rule). Weaker rules — blocking force-pushes, requiring only a
status check — are not accepted, because a collaborator with push access
could still land a commit on the branch directly, without review.

**Option A — GitHub App** (recommended for a real org: it isn't tied to any
one person's account, and scoping to specific repos is easier to audit).
Create one in the org (*Settings → Developer settings → GitHub Apps*):

- **Webhook:** off (the controller polls)
- **Repository permissions:** as listed above
- **Organization permissions:** Custom properties *read*
- Install it on **all repositories**, generate a private key, note the App ID
- Set `GITHUB_APP_ID` and `GITHUB_APP_KEY_FILE` (see `deploy/stack.yml`)

**Option B — personal access token** (simpler for a personal account or a
small setup with no time to stand up an App). Create a **fine-grained** token
(*Settings → Developer settings → Personal access tokens → Fine-grained
tokens*) scoped to the org/repos, with the same permissions as above — a
fine-grained token maps to them almost one-to-one. A classic token also
works, but needs the broad `repo` and `admin:org` scopes, since classic
tokens have no per-permission granularity.

- Set `GITHUB_TOKEN` (or `GITHUB_TOKEN_FILE` to read it from a mounted
  secret, matching how the App key is provided) instead of the two
  `GITHUB_APP_*` variables
- The token's rate limit and access follow whatever account created it —
  fine for a small number of repos, but an App scales better if the org grows
  or that person leaves

### 2. Custom property

*Org settings → Custom properties → New property*:

- Name `swarm-environments`, type **multi select**, values `sandbox`, `staging`, `prod`
- **Do not** allow repository admins to edit it: this is the approval gate

Enable environments per repo there. Removing a value removes the stack.

### 3. Branch protection

Protect `main` in every repo that deploys to prod with a rule that actually
requires a pull request: classic branch protection with "require a pull
request before merging" (and at least one required approval), or a ruleset
with a "restrict updates"/"require pull request" (`pull_request`) rule.
Rules that only block force-pushes or only require a status check are not
enough and are not accepted. Otherwise prod reports "Branch is not protected".

### 4. Deploy the controller

```sh
mkdir -p /srv/swarm                    # on EVERY node: base of the bind folders (policy bind_mounts)
docker node update --label-add swarm-gitops=true <manager>
docker secret create swarm_gitops_app_key ./app.private-key.pem
docker secret create swarm_gitops_registry ./registry-config.json   # {"auths":{"ghcr.io":{"auth":"<base64 user:token>"}}}
docker stack deploy -c deploy/stack.yml swarm-gitops
```

Bind folders are created by a short Swarm job (`mode: global-job`, named
`sg-prep-<stack>-N`). It runs `PREP_IMAGE` (the controller image) on every
node the service may be placed on, with only the base folder (e.g.
`/srv/swarm`) mounted. If a node is down, its folders get created on the next
deploy.

Start with `DRY_RUN=true`: everything is scanned, rendered, checked and
reported on GitHub (as "Dry run: would deploy"), but nothing on the Swarm
changes. Switch to `false` once that looks right.

### 5. Private registries

One `config.json` (the standard Docker CLI credentials format), mounted as a
secret, provides pull auth for everything the controller does: `docker stack
deploy`/`service update` for app stacks (`--with-registry-auth` — standard
Swarm behavior, the manager forwards the encrypted credentials to whichever
nodes pull the image), the automatic image-digest polling (talks to the
registry API directly, so it reads the same file itself), and pulling
`PREP_IMAGE` if that one is private too.

```sh
docker secret create swarm_gitops_registry ./registry-config.json
```
```json
{"auths":{"ghcr.io":{"auth":"<base64 user:token>"}}}
```

Generate the `auth` value with `echo -n 'user:token' | base64`, or reuse an
existing login: after `docker login ghcr.io`, the entry is already sitting in
`~/.docker/config.json`. Add one entry per host to support more than one
registry.

This credential is **shared by every stack the controller deploys, not
scoped per repo.** The actual access control is `image_prefixes` in the
policy — it decides which registry paths a repo's compose file may even
reference; this file is just the pull key for whichever of those images turn
out to be private. Scope the token no wider than `image_prefixes` allows
(e.g. a `read:packages` token limited to `ghcr.io/your-org/*`, matching
`image_prefixes: [ghcr.io/your-org/]`) and there's no privilege gap.

This is separate from the controller's own *first* deploy: running `docker
stack deploy -c deploy/stack.yml swarm-gitops` by hand uses the admin's own
local Docker login, not this secret (which doesn't exist yet at that point).
Only matters if you fork the image into a private registry.

### 6. Migrating from Portainer

The controller never touches stacks without its labels. If a repo's stack
name matches an existing Portainer stack, the check reports "Stack name is
taken". Take it over explicitly:

```sh
swarm-gitops adopt einsatz-app-prod
```

Then remove the stack from Portainer's Git settings, so the two don't fight.

Hostnames routed by Portainer stacks are respected automatically: nobody
else can claim them. When moving bind-mount data, copy it to
`/srv/swarm/<repo>/<env>/…` first.

### 7. CLI

The admin API is a Unix socket inside the container, not reachable over the network:

```sh
alias sg='docker exec $(docker ps -q -f label=com.docker.swarm.service.name=swarm-gitops_controller) swarm-gitops'

sg status                    # stacks, commits, pending removals, orphans
sg sync                      # scan now
sg redeploy <stack>          # force redeploy of the current commit
sg pause <stack>             # no deploys, image updates or removal
sg resume <stack>
sg adopt <stack>             # take over an unmanaged stack
sg approve-prune             # allow a blocked mass removal once
sg reset-binds /srv/swarm/app/prod   # after checking the folder for symlinks: forget its writable-mount history
```

### 8. Secrets plugin (optional)

Skip this if you're using plain `external: true` secrets (the default,
`allow_external_secrets: true` — no plugin needed, see [Secrets](#secrets)).
It's only for per-repo/per-env scoped secrets via your own Docker secrets
driver plugin.

Every managed service **and** secret carries labels that only the controller
can set (devs setting them is a policy violation):

```
swarm-gitops.managed=true
swarm-gitops.repo=<repo>
swarm-gitops.env=<env>
swarm-gitops.stack=<stack>
```

Docker passes `SecretLabels` and `ServiceLabels` to secret driver plugins.
The plugin should only hand out secrets under a path matching repo + env
(e.g. `einsatz-app/prod/db_password`). Then staging can never read prod secrets.

### Configuration

| Variable | Default | |
|---|---|---|
| `GITHUB_ORG` | – | org to scan |
| `GITHUB_APP_ID`, `GITHUB_APP_KEY_FILE` | – | GitHub App auth (Option A); mutually exclusive with the token below |
| `GITHUB_TOKEN` or `GITHUB_TOKEN_FILE` | – | personal access token auth (Option B) |
| `PREP_IMAGE` | – | required; your own image, e.g. `ghcr.io/your-org/swarm-gitops:latest` (usually the same image as the controller, see `deploy/stack.yml`) |
| `GITHUB_API_URL` | `https://api.github.com` | |
| `ENV_PROPERTY` | `swarm-environments` | custom property; empty = all envs allowed |
| `REQUIRE_PROTECTED` | `prod` | comma list of envs needing protected branches |
| `SCAN_INTERVAL` | `1m` | |
| `IMAGE_INTERVAL` | `2m` | `0` disables automatic image updates |
| `ROLLOUT_TIMEOUT` | `5m` | |
| `CI_WAIT_TIMEOUT` | `30m` | how long a commit waits for its CI before it is deployed anyway; `0` disables waiting (see [Waiting for CI](#waiting-for-ci)) |
| `PRUNE_ENABLED` | `true` | `false` = only log what would be removed |
| `PRUNE_CONFIRMATIONS` | `2` | consecutive scans before a removal |
| `MAX_PRUNE` | `3` | more removals in one scan need `approve-prune` |
| `CONCURRENCY` | `4` | parallel deployments |
| `POLICY_FILE` | `/etc/swarm-gitops/policy.yml` | |
| `DOCKER_CONFIG` | – | dir with `config.json` for registry auth |
| `PREP_TIMEOUT` | `2m` | |
| `DRY_RUN` | `false` | |
| `LOG_LEVEL` | `info` | `debug` |
| `METRICS_ADDR` | – | e.g. `:9090`; empty disables the Prometheus endpoint |

---

## Removal safety

Removal only happens on **positive evidence** from a successful read:

| Situation | Result |
|---|---|
| Repo archived | remove all its stacks |
| Repo visible, deploy.yml / env / branch / `.swarm/` / compose files gone | remove that stack |
| Env no longer enabled by an admin | remove that stack |
| GitHub error, rate limit, timeout | nothing is removed (whole scan aborted) |
| Error reading one repo's branches or files | that repo's stacks are kept |
| Broken `deploy.yml` | stacks kept, failed check "Invalid .swarm/deploy.yml" |
| Repo not visible (deleted, renamed, App access removed) | kept as **orphaned**, shown in `status` |

On top of that:

- A removal needs `PRUNE_CONFIRMATIONS` consecutive scans that agree.
- More than `MAX_PRUNE` removals in one scan stop and wait for `approve-prune`.
- Only stacks labelled `swarm-gitops.managed=true` are ever removed.
- Volumes are always kept.

## Operations

- **Hostname and Traefik name check:** the claims are read live from all
  Swarm services right before `docker stack deploy`, under a lock, so two
  stacks can't claim the same hostname or router/service/middleware name at
  the same time.
- **State:** Swarm service labels hold what runs (repo, env, commit, spec
  hash). `/data/state.json` holds only processed commits, pauses and adoptions.
  Losing it is harmless: each stack then gets one "No changes" check.
- **Controller down:** running apps are unaffected; deploys wait.
- **Temporary errors** (GitHub/Docker unreachable) are retried 3× per commit.
  After that, push an empty commit or run `redeploy`.
- **Config changes:** Swarm configs are immutable, so the controller names
  them by content hash. A changed file creates a new config and rolls the
  service. Unused old configs are pruned; the previous one is kept for rollbacks.
- **Rate limits:** a scan costs 1 GraphQL query per 50 repos, plus 1 per repo
  that has a `deploy.yml`. REST reads use ETags; unchanged responses don't
  count against the limit.
- **Metrics:** set `METRICS_ADDR` (e.g. `:9090`) to expose a read-only
  Prometheus endpoint on its own listener — deliberately separate from the
  admin API, which stays Unix-socket-only. It reports per-service desired/
  running replica counts and the controller's own scan health (last scan
  time/error, orphaned stacks, prune-blocked stacks); nothing about node or
  container resource usage, which belongs to your metrics stack, not this
  controller. Keep it off the public internet — no Traefik router, an
  internal-only overlay network is enough for something like Prometheus/
  VictoriaMetrics to scrape it.

## Known limitations

- Environments deploy from **branches**, not tags.
- Registry auth reads `auths` from `config.json`; credential helpers are not
  supported.
- Hostname ownership is "first come, first served" among running stacks.
  To give a hostname to a repo before it deploys, reserve it in the policy.
- Bind-mount history (`/data/state.json`) starts empty. Folders mounted
  writable by stacks before swarm-gitops (e.g. adopted Portainer stacks) are
  not in it; check them for symlinks when adopting.
- One controller instance, pinned to one manager (`/data` is a local volume).

## Development

Open the repo in the provided [dev container](.devcontainer/devcontainer.json)
(VS Code "Reopen in Container", or GitHub Codespaces) for Go, the Docker CLI
and a one-node Swarm ready to go — skip straight to `go test ./...` below.
Setting it up by hand needs Go 1.24+ and Docker with Swarm mode.

```sh
go test ./...                                   # unit tests (render tests need the docker CLI)
docker swarm init                               # once
internal/controller/testdata/web/build.sh      # test images (web + prepare job), no registry needed
go test -tags integration -v ./internal/controller/   # end-to-end on a real Swarm
go run ./cmd/swarm-gitops check -policy deploy/policy.yml examples/einsatz-app/.swarm
```

Only dependency: `gopkg.in/yaml.v3`.
