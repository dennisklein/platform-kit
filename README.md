# platform-kit

platform-kit is a tool to change the desired state of a multi-cluster platform, from bare metal through Kubernetes up to apps. The desired state lives in one git state repo, and Flux applies it to the clusters. Each change is one worktree, one branch and one merge request. The **bench** (one Go binary with a Svelte web UI, a CLI and an MCP endpoint for agents) renders each change for every cluster, shows its impact, runs a change lab on emulated hardware, checks the policy and follows the change to the cluster. This repository is the hands-on demo of the design: see [docs/design.html](docs/design.html) for the design and [docs/demo.html](docs/demo.html) for the demo script.

## Quick start

You need Docker (Docker Desktop on macOS, or Docker Engine 23+ with the compose plugin on Linux). The image builds for amd64 and arm64.

```sh
git clone https://github.com/dennisklein/platform-kit && cd platform-kit
make up                       # or: docker compose up -d. First time: builds the image (2–4 min), seeds Gitea (~40 s)
open http://127.0.0.1:8080    # Linux: xdg-open
```

`make up` checks first that the ports 8080, 3000 and 8081 are free (`make preflight`). If `docker compose up` fails with "address already in use", see the port note below the table.

You are logged in as **dennis** (demo mode). The other services:

| Service | URL | Notes |
|---|---|---|
| bench (UI, API, MCP) | http://127.0.0.1:8080 | API under `/api/v1`, MCP at `/mcp` |
| Gitea (forge) | http://127.0.0.1:3000 | users `dennis`, `anna`, `jonas`, `mara`, `vera`, password **`pk-demo-2026`**; admin `pk-admin` / `pk-admin-demo` |
| fluxsim (simulated Flux) | http://127.0.0.1:8081/status | |

All ports bind to 127.0.0.1 only. If a port is in use, set `PK_BENCH_PORT`, `PK_FORGE_PORT` or `PK_FLUX_PORT` (in the shell or in a `.env` file next to `compose.yaml`), for example `PK_BENCH_PORT=8090`. Port 3000 is often in use on developer laptops (Grafana, Node dev servers). `make up` and `make fresh` find a busy port at once and name the variable to set; a plain `docker compose up` finds a busy bench port only after the forge seeding (about 40 s).

Times that the containers print (for example "approved until 18:30") use the zone in `PK_TZ` (default `Europe/Berlin`). Gitea logs only fatal errors (its "already in queue" errors during the seeding are harmless and hidden).

After a `git pull`, rebuild the image: `docker compose up -d --build` (plain `up` reuses the image it has). `make up` does the same and waits until all services are healthy.

The CLI is in the image: `docker compose exec bench pk change list` (try `alias pk='docker compose exec -T bench pk'`).

## What is real and what is simulated

| Part | In this demo |
|---|---|
| Changes, worktrees, branches, commits, rebase, merge with trailers | real (git CLI in the bench) |
| Forge | real Gitea 1.24 (pull requests, reviews, commit statuses, branch protection), in place of GitLab |
| Policy check (`pk ci`, status `pk/policy`), path rights, approvals, lab gate | real |
| Render, diff and impact | real: Kustomize builds in process, kind hooks render Nodes to Machines. **Helm charts are not templated**: a HelmRelease is compared as an object. |
| Schemas, form editor, cross-file checks (ports, units, cables) | real |
| Secrets (SOPS) | masked, **never decrypted**; a save keeps the stored ciphertext. Decrypt and re-encrypt with a local admin key are not built. |
| Change labs | **simulated** driver (no VMs). The steps take simulated time. The **checks are real** computations on the rendered desired state of the change (nodes Ready, BGP coverage, BMC fixture, link fault). The report article and `pk/lab` status are real. |
| Flux and the clusters | **simulated** by `fluxsim`: it follows main (or a ring tag), renders each cluster and reports applied and verified revisions. Health checks fail only on a demo fault (Clusters, "Make it fail"). |
| Digital twin `defiant-twin` | simulated by fluxsim |
| Rings, promote, hold, revert | real git tags and changes; the clusters behind them are fluxsim |
| LLM: advisors, chat, commit messages, LLM log, redaction | real with an OpenAI-compatible endpoint; deterministic **mock model** without one |
| MCP for agent harnesses | real (Streamable HTTP, tools generated from the OpenAPI spec) |
| Org database sync | not built (the sync-bot changes are seeded) |
| Shared service mode (SSO, review in the browser) | not built; the bench runs in local mode |

## LLM setup

Without configuration the bench uses the **mock model**: answers come from the same deterministic findings, phrased by templates. The top bar shows "LLM mock".

For a real model, give the bench an OpenAI-compatible endpoint before `up`:

| Variable | Meaning |
|---|---|
| `PK_LLM_BASE_URL` | base URL with `/v1`, for example `http://host.docker.internal:11434/v1` |
| `PK_LLM_MODEL` | model name; set it (default: the first model the endpoint lists) |
| `PK_LLM_API_KEY` | bearer token, if the endpoint needs one |
| `PK_LLM_EMBED_MODEL` | optional embedding model; without it retrieval uses BM25 keywords |
| `PK_LLM_TIMEOUT` | longest time for one model call, as a Go duration (default `45s`); after a failed call the bench answers from its data for 30 s |

`host.docker.internal` is the machine that runs Docker (compose maps it with `host-gateway`, so it works on macOS and Linux).

Example with Ollama on the same machine:

```sh
ollama pull qwen3:14b                      # any chat model with tool calls
# Linux only: Ollama must listen on the docker bridge, not only on 127.0.0.1
#   sudo systemctl edit ollama  ->  Environment="OLLAMA_HOST=0.0.0.0:11434"
cat > .env <<'EOF'
PK_LLM_BASE_URL=http://host.docker.internal:11434/v1
PK_LLM_MODEL=qwen3:14b
EOF
docker compose up -d        # recreates the bench with the new settings
make llm-check              # lists the models and asks for one answer from inside the bench container
```

The bench removes secrets before each call (SOPS values, schema secret fields, private keys, tokens). Every call goes to the LLM log (page "LLM log"). On a model error the advisors fall back to their deterministic findings.

## Agent setup (MCP)

1. In the bench, open **Settings** and click **Approve agent for today** (later: **Manage agents**). Choose the mode (Navigate: your screen follows the agent; Suggest: the agent sends "look here" cards) and the time (1 h to 12 h). Click **Approve agent for today**.
2. The page shows the token once, with snippets (**Copy command**, **Copy allow rule**, **Copy .mcp.json**, **Copy opencode.json**; `pk agents approve` prints the same). Copy one into your agent harness:

   Claude Code (in the project directory of the agent):

   ```sh
   claude mcp add --transport http platform-kit http://127.0.0.1:8080/mcp \
     --header "Authorization: Bearer pk_agt_…"
   # Allow the platform-kit tools in this directory. Without it, Claude Code
   # asks for permission at each new tool (about 8 times for one change).
   mkdir -p .claude && echo '{"permissions":{"allow":["mcp__platform-kit"]}}' > .claude/settings.local.json
   ```

   Other way for the permissions: start the agent with `claude --allowedTools mcp__platform-kit`. opencode allows MCP tools without a prompt.

   opencode (`opencode.json`; the provider block is an example for a local Ollama model):

   ```json
   {
     "$schema": "https://opencode.ai/config.json",
     "provider": {
       "ollama": {
         "npm": "@ai-sdk/openai-compatible",
         "name": "Ollama (local)",
         "options": { "baseURL": "http://localhost:11434/v1" },
         "models": { "qwen3:14b": { "name": "qwen3 14b" } }
       }
     },
     "mcp": {
       "platform-kit": {
         "type": "remote",
         "url": "http://127.0.0.1:8080/mcp",
         "enabled": true,
         "headers": { "Authorization": "Bearer pk_agt_…" }
       }
     }
   }
   ```

3. Ask the agent for a change, for example: *"Add cn-037 to defiant in GC5A-6Z U30 with free ports, run the lab, then make it ready for review."* The change appears in the web UI at once, with a presence chip. The agent commits as `dennis-agent` (trailer `Pk-Agent`). It can push, run labs and mark ready; `pk_change_approve` and `pk_change_merge` are denied.

The token is valid until the time on the page. `make fresh` deletes it; approve the agent again after that.

Without an agent harness, `scripts/mcp-demo.sh pk_agt_…` sends the same MCP calls as an agent for "add cn-037" (new change, node file, commit, lab, denied approve). The web UI shows them the same way.

`make e2e` approves `dennis-agent` for its own check and revokes the grant at the end. When the agent already has an active grant, it skips that check, so your token keeps working (`E2E_AGENT_FORCE=1` runs it anyway).

## Rehearsal reset

| Command | What it does | Time |
|---|---|---|
| `make reset` (`docker compose run --rm forge-init forge-init --reset`) | Deletes and rebuilds the Gitea repo with the seed history and the 4 open changes. The bench drops the worktrees and lab runs of the old state; ci and fluxsim follow. Nothing restarts. Agent grants and the LLM log stay. | ~35 s |
| `make fresh` (`docker compose down -v && docker compose up -d --wait`) | Deletes all data (Gitea, bench, LLM log, agent tokens, lab runs) and starts again. Use it before the talk. | ~45 s |

Reload the browser tab after a reset.

## Development

Go 1.24 (`GOTOOLCHAIN=local`), Node 22, npm, Docker.

| Target | What it does |
|---|---|
| `make gen` | Go server, Go client and TS types from `api/openapi.yaml` (run it after each change to the spec) |
| `make web` | build the SPA into `web/dist` (embedded into the binary) |
| `make build` | build `bin/pk` |
| `make test` | Go tests, web unit tests and `svelte-check` |
| `make run` | the bench from source on http://127.0.0.1:8090, with Gitea, ci and fluxsim from compose |
| `make web-dev` | Vite dev server on http://localhost:5173 with hot reload, proxied to the bench of `make run` |
| `make up` / `make down` | the compose stack (check the ports, build and start / stop and keep data) |
| `make preflight` | check that the host ports of bench, Gitea and fluxsim are free (`scripts/preflight-ports.sh`) |
| `make status`, `make logs` | containers and bench status; follow the logs |
| `make e2e` | the demo story through the API against compose (`scripts/e2e.sh`), then the Playwright smoke test |
| `make docker` | build the image (`docker buildx build --platform linux/amd64,linux/arm64 .` for both) |
| `scripts/clone-test.sh [repo] [branch]` | the Quick start in a temporary clone, on other ports (project `pkclone`): build, up, checks, then delete |

`?mock=1` in the page URL (before the `#`) runs the web UI on fixture data without a bench.

## Release check (before you share or present)

The Quick start clones the default branch. Everything that the demo needs must be on it:

1. Commit the whole working tree (`git status` must be clean; `README.md`, `docs/demo.html` and `scripts/*.sh` are new files). Do not commit the old binary `pk` in the root: `git rm --cached pk` (`/pk` is in `.gitignore`).
2. Push and merge to `main` (or make the demo branch the default branch; else add `-b <branch>` to the clone command above).
3. On the laptop of the talk, the day before: `scripts/clone-test.sh`. It clones the repository like the Quick start, builds and starts it on other ports, checks the bench, the seeded changes, Gitea, fluxsim and the CLI, then deletes everything. It must end with `clone test: ok`.

## Repository layout

```
api/openapi.yaml        the API (OpenAPI 3.0.3); x-mcp-tool marks the MCP tools
cmd/pk/                 the pk binary: bench, ci, fluxsim, forge-init and the generated CLI
internal/bench/         HTTP server, auth, SSE, focus, handlers per area
internal/change/        change model: worktrees, state, trailers, rebase, overlap (gitx: git CLI)
internal/forge/         forge interface, Gitea adapter, in-memory fake
internal/kinds/         kind registry: Node, Rack, Switch, Article, WorkItem, ... and third-party overlays
internal/schema/        JSON Schemas with x-pk extensions, validation, masking
internal/render/        render per cluster, diff, impact, semantic diff
internal/policy/        .pk/policy.yaml: path rights, approvals, lab gate, risk
internal/lab/           change labs: topology, scenario, checks, report (sim driver)
internal/fluxsim/       simulated Flux; internal/rings: ring tags and gates
internal/llm/           OpenAI-compatible client, mock, redaction, LLM log, retrieval index
internal/advisor/, chat/, mcp/   advisor recipes, chat with suggestions, MCP server
internal/seed/, seed/   the demo state repo and its history, pushed by forge-init
web/                    Svelte 5 SPA (src/routes per page, src/lib/api generated client)
docs/                   design, demo script, lab examples, mocks
scripts/                e2e.sh (demo story), llm-check.sh, mcp-demo.sh (agent calls without a harness),
                        preflight-ports.sh (make up), clone-test.sh (release check)
Dockerfile, compose.yaml
```
