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
   no published ports, secrets only via the plugin, …).
5. **Deploy**: only if the rendered stack changed (or on an empty commit),
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

Compose never interpolates label **keys**. The controller replaces `${STACK}`
and `${ENV}` in label keys, so Traefik router names stay unique per stack:
`traefik.http.routers.${STACK}.rule: Host(\`${DOMAIN}\`)`.

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

If `stack.yml` uses a tag (`ghcr.io/bergwacht-bayern/app:main`), a new image
pushed to that tag is rolled out automatically within ~2 minutes. It shows up
as a deployment "New image: app → sha256:…" in the repo. To opt out, pin
by digest (`app:1.4.2@sha256:…`); pinned images are never touched.

Use `update_config.failure_action: rollback`, so a broken image rolls back
automatically and is reported as failed.

### Secrets

Secrets are never in git. Declare them with the secrets plugin as driver:

```yaml
secrets:
  db_password:
    driver: bergwacht/secrets
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
| Bind mounts (incl. `docker.sock`), `driver_opts` bind tricks | host takeover |
| `privileged`, `cap_add`, `network_mode`, `pid`, `ipc`, `devices`, `security_opt` | host takeover |
| Published `ports` | bypasses Traefik, port conflicts |
| Images outside `ghcr.io/bergwacht-bayern/` and official images | supply chain |
| External networks other than `traefik-public` | isolation between stacks |
| External volumes/configs/secrets, custom `name:` | access to other stacks' data |
| Secrets without the plugin driver | secrets in git |
| `env_file`/`file` paths outside `.swarm/` | reading controller files |
| Labels starting with `swarm-gitops.` | reserved for the controller |

The policy lives in [`deploy/policy.yml`](deploy/policy.yml).

---

## For admins

### 1. GitHub App

Create an App in the org (*Settings → Developer settings → GitHub Apps*):

- **Webhook:** off (the controller polls)
- **Repository permissions:** Contents *read*, Metadata *read*, Checks *read & write*,
  Deployments *read & write*
- **Organization permissions:** Custom properties *read*
- Install it on **all repositories**, generate a private key, note the App ID.

### 2. Custom property

*Org settings → Custom properties → New property*:

- Name `swarm-environments`, type **multi select**, values `sandbox`, `staging`, `prod`
- **Do not** allow repository admins to edit it: this is the approval gate

Enable environments per repo there. Removing a value removes the stack.

### 3. Branch protection

Protect `main` (branch protection or a ruleset with "require pull request") in
every repo that deploys to prod. Otherwise prod reports "Branch is not protected".

### 4. Deploy the controller

```sh
docker node update --label-add swarm-gitops=true <manager>
docker secret create swarm_gitops_app_key ./app.private-key.pem
docker secret create swarm_gitops_registry ./registry-config.json   # {"auths":{"ghcr.io":{"auth":"<base64 user:token>"}}}
docker stack deploy -c deploy/stack.yml swarm-gitops
```

Start with `DRY_RUN=true`: everything is scanned, rendered, checked and
reported on GitHub (as "Dry run: would deploy"), but nothing on the Swarm
changes. Switch to `false` once that looks right.

### 5. Migrating from Portainer

The controller never touches stacks without its labels. If a repo's stack
name matches an existing Portainer stack, the check reports "Stack name is
taken". Take it over explicitly:

```sh
swarm-gitops adopt einsatz-app-prod
```

Then remove the stack from Portainer's Git settings, so the two don't fight.

### 6. CLI

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
```

### 7. Secrets plugin

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
| `GITHUB_APP_ID`, `GITHUB_APP_KEY_FILE` | – | App credentials |
| `GITHUB_API_URL` | `https://api.github.com` | |
| `ENV_PROPERTY` | `swarm-environments` | custom property; empty = all envs allowed |
| `REQUIRE_PROTECTED` | `prod` | comma list of envs needing protected branches |
| `SCAN_INTERVAL` | `1m` | |
| `IMAGE_INTERVAL` | `2m` | `0` disables automatic image updates |
| `ROLLOUT_TIMEOUT` | `5m` | |
| `PRUNE_ENABLED` | `true` | `false` = only log what would be removed |
| `PRUNE_CONFIRMATIONS` | `2` | consecutive scans before a removal |
| `MAX_PRUNE` | `3` | more removals in one scan need `approve-prune` |
| `CONCURRENCY` | `4` | parallel deployments |
| `POLICY_FILE` | `/etc/swarm-gitops/policy.yml` | |
| `DOCKER_CONFIG` | – | dir with `config.json` for registry auth |
| `DRY_RUN` | `false` | |
| `LOG_LEVEL` | `info` | `debug` |

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

## Known limitations

- Environments deploy from **branches**, not tags.
- Registry auth reads `auths` from `config.json`; credential helpers are not
  supported.
- The policy does not stop one stack from claiming another stack's hostname
  in a Traefik `Host()` rule. If that matters, add a per-environment domain
  rule to the policy.
- One controller instance, pinned to one manager (`/data` is a local volume).

## Development

```sh
go test ./...                                   # unit tests (render tests need the docker CLI)
docker swarm init                               # once
internal/controller/testdata/web/build.sh      # test images, no registry needed
go test -tags integration -v ./internal/controller/   # end-to-end on a real Swarm
go run ./cmd/swarm-gitops check -policy deploy/policy.yml examples/einsatz-app/.swarm
```

Only dependency: `gopkg.in/yaml.v3`.
