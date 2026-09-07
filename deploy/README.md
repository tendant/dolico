# Deploying dolico as an internal service

Two containers on one host: the API server and the OCR tier. The vision tier is
not included — see *Turning on the vision tier* below.

```bash
make image      # dolico-api:<commit>, dolico-ocr:<commit>
make up
curl -F file=@testdata/mixed.pdf 'http://127.0.0.1:8080/v1/documents?wait=true'
```

`make image` names what it builds `dolico-api:<commit>`, with no registry
prefix at all — an image built here and never pushed has no business carrying
the name of a registry it did not come from. Set `DOLICO_REGISTRY` when you do
mean to push or pull, and the same names gain the prefix:

```bash
DOLICO_REGISTRY=reg.memochat.ai make image   # reg.memochat.ai/dolico-api:39d18d6
```

**The tag is the commit, and you do not set it.** It is the same seven
characters CI tags with, so an image built here and one built there have the
same name for the same code, and anything pulled from a registry can be checked
out. A working tree with changes in it — untracked files included, since those
reach a build context just as readily — tags `<commit>-dirty` instead, because
the commit alone would name something that was never built. Outside a git
checkout it falls back to `dev`, which claims nothing.

`DOLICO_TAG` still overrides it, from the environment or `deploy/.env`, for the
times you mean something other than "this commit".

No registry is written into the compose file. This repository is public, so one
hardcoded there would be wrong for most people reading it and a standing
invitation to push somewhere by accident.

Calling compose directly still works and behaves identically — an unset
`DOLICO_REGISTRY` is simply no prefix:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

The first start is slow and *reports itself unhealthy while it is*: PaddleOCR
downloads a few hundred megabytes of models and loads them once per worker, and
`/healthz` returns 503 until that finishes. The API deliberately refuses to
start until the OCR service is healthy, so `docker compose up` will sit there
for a few minutes the first time and start quickly on every restart after,
because the models live in a volume.

## Or pull prebuilt images instead of building

Building here is for the machine that produces the images. Every other host
should pull them: the OCR image is ~4.2GB and pulls the PaddlePaddle wheels,
and building it on each server is minutes of CPU to arrive at bytes CI already
produced.

`dolico-stack/ci/pipeline.hcl` builds both images on every push to `main` and
pushes them to `reg.memochat.ai` tagged with the 7-character commit SHA, plus a
moving `latest`. So a new host needs this compose file and a registry login —
not a clone, a Go toolchain or a Rust one:

```bash
docker login reg.memochat.ai
export DOLICO_REGISTRY=reg.memochat.ai
export DOLICO_TAG=39d18d6            # the commit you mean to run
docker compose -f docker-compose.yml pull
docker compose -f docker-compose.yml up -d --no-build
```

Both variables, and both in the `.env` beside the compose file on a server that
will restart unattended. `DOLICO_REGISTRY` is what turns `dolico-api:39d18d6`
into something pullable; without it compose looks for a local image by that
bare name, finds none, and — because the service has a build section — tries to
build it, which is what `--no-build` is there to turn into an error.

**Pin `DOLICO_TAG`.** The compose file defaults it to `latest` so that a bare
`docker compose` invocation resolves to something at all — `make` sets the
current commit and never consults that default. A server left on `latest` with `restart: unless-stopped`
picks up a different version on its next reboot, which is a version change
nobody performed and nobody logged.

**Pass `--no-build`.** A service that has a build section as well as an image
name is built, not pulled, when compose cannot find the image — so a typo in
the tag turns a pull into a silent local build rather than an error. `--no-build`
makes it an error.

`DOLICO_REGISTRY` overrides the registry host if the images live somewhere else.

## Pushing images to a registry

`deploy/.env` names the registry, and building and pushing then agree by
construction rather than by remembering to pass the same two variables twice:

```bash
# deploy/.env -- gitignored
DOLICO_REGISTRY=reg.memochat.ai
DOLICO_REGISTRY_USER=tendant
DOLICO_REGISTRY_PASSWORD=...
```

No `DOLICO_TAG` here: it defaults to the commit being built, which is what you
want a pushed image tagged with anyway.

```bash
make login    # only when the host is not logged in already
make image
make push
```

**`push` does not build.** It sends the images that are already there, so what
reaches the registry is what you tested — a push that built could pick up a
base image that moved under `--pull`, or a file edited since.

It does check they exist first, because compose's own failure is misleading:

```
✘ reg.example.com/dolico-ocr:f7cbc42  tag does not exist: reg.example.com/dolico-api:f7cbc42
```

Two rows, one error message rendered against both — nothing was pushed under
another image's name. `make push` says which names are missing instead, and if
an unprefixed `dolico-api:<commit>` is sitting there it says that too, since
building before `DOLICO_REGISTRY` was set is how you get there. `make image`
re-tags in a cache hit.

`make push` refuses while `DOLICO_REGISTRY` is unset, since the images would
carry no registry in their names and have nowhere to go.

**The password is optional and does not have to be there at all.** With no
`DOLICO_REGISTRY_PASSWORD`, `make login` runs an ordinary interactive
`docker login`, which leaves the credential in docker's own store and nothing
on disk in this repository. That is the better arrangement wherever a person is
present to type it; the variable exists for hosts where none is.

When it is set, it reaches docker on stdin — never as an argument, which every
other user on the host can read out of `ps`, and never through a make variable,
which `make -n` would print. `scripts/registry-login` reads the file rather than
sourcing it, so a `$` or a backtick in a password is sent as written instead of
being expanded or executed.

`docker push` is still the only command in this repository that contacts a
registry, and `make push` is still the only target that runs it. CI does its own
pushing from `dolico-stack/ci/pipeline.hcl`, with no password anywhere in it:
that task mounts the host's docker config read-only and reuses the login
already on it.

## The OCR image is amd64 only

PaddlePaddle publishes no Linux aarch64 wheels — PyPI has `manylinux1_x86_64`,
`macosx_11_0_arm64` and `win_amd64`, and that is the whole list. The compose
file therefore pins the OCR service to `linux/amd64`:

```yaml
platform: ${DOLICO_OCR_PLATFORM:-linux/amd64}
```

On an amd64 server this costs nothing. On an Apple Silicon machine it runs
under emulation, and since Paddle is CPU-bound numeric code, **expect it to be
several times slower than the `make ocr` you develop against** — that path uses
the native macOS arm64 wheel. Measured here: ~7s for a page that takes ~2.5s
natively. Emulated, it works; it is not a performance measurement.

## oneDNN is not only an Apple Silicon problem

**Turn oneDNN off unless you have measured that your host does not need it:**

```bash
DOLICO_PADDLE_MKLDNN=False docker compose -f deploy/docker-compose.yml up -d
```

Inference dies inside the model runner with
`ConvertPirAttribute2RuntimeAttribute not support`, which reaches the API as a
bare `500` from `/v1/extract` and says nothing about the cause. The API handles
it correctly — the page comes back with `ocr_failed` in its reasons and the
error in `trace.engines` — but **every scanned page is empty while the service
looks healthy**: `/healthz` stays green and `/v1/engines` still lists
`pp-structurev3`. Nothing short of running a document through it says otherwise,
which is what `make verify` is for.

This was documented here as an emulation artifact — something that happened on
an arm64 machine running the amd64 image — with the advice to leave the flag
alone on a real server. That advice is wrong, and following it is how a
deployment ends up serving empty pages while every health signal says it is
working. Measured on a native x86_64 host (Xeon E5-2620 v4, AVX2 without
AVX512, under VMware, paddle 3.3.1), driving `StructureEngine` directly inside
the container:

| `DOLICO_PADDLE_MKLDNN` | thread | result |
| --- | --- | --- |
| `False` | main | 4 blocks read |
| `False` | worker | 4 blocks read |
| `True` | main | `NotImplementedError: ConvertPirAttribute2RuntimeAttribute` |
| `True` | worker | `NotImplementedError: ConvertPirAttribute2RuntimeAttribute` |

`paddle.utils.run_check()` passes on that host, so it is neither a broken
install nor an unsupported CPU. The failure is in PIR's oneDNN instruction path
(`.../new_executor/instruction/onednn/onednn_instruction.cc:116`), which
emulation reaches for its own reasons and native amd64 reaches directly.

oneDNN remains the faster path where it works. The point is that "amd64" is not
evidence that it works — a document through `make verify` is.

The API image has no such constraint and builds natively for the host.

## Building behind a slow index

The Rust stage fetches crates from `crates.io`, which on some networks is slow
enough to dominate the build or unreachable outright. `CARGO_REGISTRY_MIRROR`
points it somewhere else:

```bash
CARGO_REGISTRY_MIRROR=sparse+https://mirrors.ustc.edu.cn/crates.io-index/ make image
```

Or, since a mirror is a property of where you are and not of the build, put it
in `deploy/.env` so every later `make image` picks it up:

```bash
# deploy/.env -- gitignored
CARGO_REGISTRY_MIRROR=sparse+https://mirrors.ustc.edu.cn/crates.io-index/
PYPI_FILES_MIRROR=https://pypi.tuna.tsinghua.edu.cn
PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
DEBIAN_MIRROR=https://mirrors.tuna.tsinghua.edu.cn
```

Compose reads that file on its own, because it sits beside the compose file —
so it is also where a server holding only `docker-compose.yml` puts its own
settings, with no flag to remember on either side.

### The Python side needs two variables, and `index-url` is the wrong one

The OCR image is where a mirror actually pays: `uv sync` pulls PaddlePaddle and
its dependencies, which is most of a 4.2GB image. But setting only an index URL
there does nothing at all, silently.

`uv sync --frozen` never consults an index. `uv.lock` pins an absolute
`https://files.pythonhosted.org/...` URL for every one of its 1272 wheels and
sdists, and uv downloads exactly those. Point `PIP_INDEX_URL` at Tsinghua,
block `files.pythonhosted.org`, and the build still fails on a connection to
PyPI — which is the whole mechanism in one sentence.

`PYPI_FILES_MIRROR` is the one that works. It rewrites that host in the lock
before uv reads it:

```bash
PYPI_FILES_MIRROR=https://pypi.tuna.tsinghua.edu.cn make image
```

The mirrors serve the same `/packages/...` paths — Tsinghua and USTC both do —
so **nothing about the resolution changes**: same versions, same files, and uv
still verifies every sha256 the lock records. A mirror serving different bytes
fails the build rather than quietly changing the image. That is the same
guarantee `--locked` gives the Rust stage, by the same means.

`PIP_INDEX_URL` is still accepted and still worth setting. It reaches `pip` and
any uv invocation that re-resolves rather than installing from the lock; it is
simply not what makes this build faster today.

**No mirror URL is committed, and none should be.** The variable is empty by
default and the Dockerfile then writes no cargo config at all, so an unset
value means upstream `crates.io` — the right default for a build machine
anywhere else. A URL checked in here would be wrong for half the people who
clone this and stale for the rest.

This is source replacement, not a different registry: crates still resolve by
the names and versions in `Cargo.lock`, and are still checked against its
hashes. `cargo build --locked` means exactly what it meant before, and a mirror
serving different bytes fails the build rather than quietly changing what it
produces. Tsinghua's index
(`sparse+https://mirrors.tuna.tsinghua.edu.cn/crates.io-index/`) works the same
way.

Nothing equivalent is needed for the Go stage: this module has no third-party
dependencies, so there is no `GOPROXY` to point anywhere.

### Debian packages

`DEBIAN_MIRROR` moves the OCR image's `apt-get` off `deb.debian.org`, which on
a bad link stalls that step for tens of minutes before it gets anywhere:

```bash
DEBIAN_MIRROR=https://mirrors.tuna.tsinghua.edu.cn make image
```

One substitution covers both suites — the mirrors lay them out exactly as the
archive does, `/debian` and `/debian-security` under the same host — and the
base image already carries CA certificates, so an `https` mirror works. The API
image needs no equivalent: its runtime stage installs nothing.

### A build step that dies with exit code 137

That is SIGKILL, and on a build it is almost always the OOM killer rather than
anything about the step it lands on. `make image` builds the two images one
after another for this reason: the api stage is a full Rust release compile
that takes every core it is given, the ocr stage unpacks several gigabytes of
PaddlePaddle wheels, and on a Docker VM with 8GB the pair does not fit. Running
`docker compose build` by hand builds them in parallel and can still hit it —
add the service name, or raise the VM's memory in Docker Desktop's settings.

**Installing the toolchain itself** — for `make build` on the host, outside
Docker — is a separate problem with separate variables, since rustup fetches
the compiler rather than any crate:

```bash
export RUSTUP_DIST_SERVER=https://mirrors.ustc.edu.cn/rust-static
export RUSTUP_UPDATE_ROOT=https://mirrors.ustc.edu.cn/rust-static/rustup
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
```

The image build needs none of that: `rust:1-bookworm` ships the toolchain
already.

## The API image installs nothing

`Dockerfile.api`'s runtime stage is `debian:bookworm-slim` with no `apt-get` in
it at all: the Go binary, the Rust shim, a data directory arriving already owned
by uid 10001, and the CA bundle copied out of the Go builder rather than
installed from the archive.

The reason is not image size. An `apt-get` in a runtime stage means the build
breaks whenever the base image's keyring is older than the keys the Debian
archive is signed with — `NO_PUBKEY`, on a line this repository did not write
and cannot fix. `make image` also passes `--pull`, so a base left in the local
store cannot go stale into that failure.

Two things follow from it:

- **There is no `curl` in the image**, so the health check is the binary:
  `dolico healthcheck` GETs its own `/healthz` and exits 0 or 1, which is what
  `HEALTHCHECK` runs. Kubernetes ignores all of this — its probes are
  `httpGet`, performed by the kubelet from outside the container.
- **A shell and `find` are still there**, because debian-slim ships them and
  removing them would buy nothing. `docker compose exec api sh` works.

Distroless would be a better fit and is not used: `gcr.io` is not reachable
from every network this gets built on. Every image referenced by either
Dockerfile is on Docker Hub, deliberately.

The OCR image goes further and does install packages. `libgl1`,
`libglib2.0-0` and `libgomp1` are opencv's and Paddle's, there is no builder
stage to copy them out of, and that image is deliberately not multi-stage
because Paddle's compiled `.so` files have baked-in paths that break when a
venv moves between stages. So `--pull` matters most for the image that cannot
be made apt-free.

## This is not safe to expose. What your gateway must do

**dolico has no authentication, no authorization and no rate limiting.** The
compose file publishes the API on `127.0.0.1` only for that reason. Anything
that can reach port 8080 can upload documents and read every document already
in the store, including other people's.

Five things whatever you put in front has to handle:

| | Why |
| --- | --- |
| **Authentication** | There is none in the application. This is the whole reason for the loopback bind. |
| **TLS** | The API speaks plain HTTP and has no certificate handling. |
| **Rate limiting / quotas** | The job queue is `workers × 16` deep and returns `503` past that, with no retry and no fairness between callers. |
| **Body size ≥ the upload cap** | `DOLICO_MAX_UPLOAD_BYTES` defaults to 256MB. A proxy with a 1MB default body limit will reject most real uploads with a confusing error. |
| **A read timeout above ~150s** | `POST /v1/documents?wait=true` blocks until the document is done, bounded by `DOLICO_SHIM_TIMEOUT + 30s`. A 60s gateway timeout cuts off exactly the large documents people care about. Callers that cannot wait should use the async path and poll `/v1/jobs/{id}`. |

There is no per-tenant separation of any kind. Document IDs are content hashes,
so anyone who can compute the hash of a file can fetch it. If two teams must not
read each other's documents, run two deployments.

## Sizing

Memory is the binding constraint, and it is dominated by the OCR service.

| | |
| --- | --- |
| OCR, per worker | **~3GB** once warm — models are ~1.5GB and allocator arenas grow |
| OCR default | 2 workers, `mem_limit: 8g` |
| API | a few hundred MB; the limit is set at 2g for headroom |

One OCR inference uses about one core and does not thread, so throughput scales
with `DOLICO_OCR_WORKERS` and nothing else. Raise it and raise `OCR_MEM_LIMIT`
with it — 4 workers wants ~14GB. `DOLICO_OCR_WORKERS` also sets the container's
uvicorn worker count, and the Go client reads the number back from the service
and matches its request concurrency automatically, so there is one knob rather
than three.

```bash
DOLICO_OCR_WORKERS=4 OCR_MEM_LIMIT=16g \
  docker compose -f deploy/docker-compose.yml up -d
```

## What survives a restart, and what does not

This matters more than it usually would, because there is no database.

- **Documents survive.** Blobs and derived documents live in the `dolico-data`
  volume. `GET /v1/documents/{id}` works after a restart, and re-uploading the
  same bytes resolves to the same document without reprocessing it.
- **Job records do not.** They live in a map. After a restart
  `GET /v1/jobs/{id}` returns 404 for every job, including ones that finished.
  The upload response contains `document_id` as well as `job_id` — clients that
  keep it can still fetch their result; clients that kept only the job ID
  cannot.
- **In-flight work is finished, not dropped**, on a graceful stop: the worker
  pool drains with a 30s deadline, then in-flight subprocesses are killed.
- **Queued-but-unstarted work is lost.** Nothing re-queues it.

The practical consequence: clients should treat a 404 from `/v1/jobs/{id}` as
"re-upload", and re-uploading is cheap because it is idempotent by content hash.

**A document stored during an outage is redone, not remembered.** If a tier is
down the document still completes — pages come back empty with `ocr_failed` in
their reasons, which is the right answer for that request — and it is written to
the store like any other. Re-uploading the same bytes reprocesses it rather than
serving the empty pages again, and the server says so:

```
INFO reprocessing a document stored with missing pages reason=ocr_failed
```

Documents that finished properly still short-circuit on the content hash, so a
re-upload of a good document is still a few milliseconds. The distinction is
whether a page is missing content an engine was supposed to produce: a page the
OCR tier *read* and found blank is finished, and a page it never managed to read
is not. Failed *vision* escalations do not trigger reprocessing either — those
pages still carry the OCR tier's text.

## The blob store grows forever

There is no retention policy, no garbage collection and no eviction. Every
document ever uploaded stays in the volume until something removes it. On a
service taking real traffic this is the first thing that will page you.

Until there is a real answer, a cron job on the host is the honest workaround:

```bash
# Delete derived documents and blobs untouched for 30 days.
docker compose -f deploy/docker-compose.yml exec api \
  find /var/lib/dolico -type f -atime +30 -delete
```

Check what that would remove before trusting it, and note that it will happily
delete a document that is still referenced by a job someone is about to poll.
`DOLICO_BLOB_TTL` does the same job from inside the process, without a cron
entry and without a second container that has to know the layout.

## Upgrading

On the build host:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

On a host running prebuilt images, an upgrade is a tag change:

```bash
DOLICO_TAG=<new-sha> docker compose -f docker-compose.yml pull
DOLICO_TAG=<new-sha> docker compose -f docker-compose.yml up -d --no-build
```

Keep `DOLICO_TAG` somewhere the next `up` will read it — an `.env` beside the
compose file — or the containers revert to whatever `latest` resolves to the
next time someone restarts them without the variable set.

Two version numbers change what happens to cached work:

- **`canonical.PipelineVersion`** participates in cache keys and in the stored
  document check. Bumping it makes every stored document reprocess on next
  upload, which is the point — it means the routing rules changed.
- **Engine versions** are part of the page cache key, so upgrading PaddleOCR
  re-runs only the pages that engine produced.

Neither is destructive: old documents stay readable, they are just recomputed
when touched.

## Turning on the vision tier

Not included by default. It roughly triples the image (torch), adds ~3.2GB of
model weights on first run, and takes memory from ~3GB to **~7GB per worker**,
because the disagreement probe means MinerU is resident for every document with
a scanned page rather than an occasional escalation.

1. In `deploy/Dockerfile.ocr`, add `--extra vision` to both `uv sync` lines.
2. Set `DOLICO_VISION_ENABLED=1` on the `api` service.
3. Raise `OCR_MEM_LIMIT` to at least `workers × 7GB` and give the model volume
   a few more gigabytes.

`docs/vision-tier-design.md` has the measurements, including what it costs on
documents that did not need it (+36% wall time on a corpus where nothing did).

`DOLICO_MINERU_URL` would let several OCR workers share one copy of the model
instead of each holding ~3GB. It is written and documented but has never been
run against a real MinerU server, so it is not wired into this compose file.

## Configuration

Every variable in the root README's Configuration table works here. The ones
this compose file exposes:

| Variable | Default | |
| --- | --- | --- |
| `CARGO_REGISTRY_MIRROR` | unset | crates.io source replacement for the Rust build stage; unset means upstream |
| `PYPI_FILES_MIRROR` | unset | host to rewrite `files.pythonhosted.org` to in `uv.lock`; the one that speeds up the OCR build |
| `PIP_INDEX_URL` | unset | PyPI index for pip and for re-resolving; not consulted by `uv sync --frozen` |
| `DEBIAN_MIRROR` | unset | replaces `deb.debian.org` in the OCR image's apt sources |
| `DOLICO_REGISTRY` | unset | registry prefix on the image names; unset means a bare `dolico-api:tag`, built and never pushed |
| `DOLICO_TAG` | `latest` | image tag; under `make` it defaults to the current commit (`-dirty` if the tree is). Pin it on a server |
| `DOLICO_REGISTRY_USER` | unset | registry username, for `make login` |
| `DOLICO_REGISTRY_PASSWORD` | unset | registry password; unset means `make login` prompts |
| `DOLICO_PORT` | `8080` | host port, bound to `127.0.0.1` |
| `DOLICO_OCR_WORKERS` | `2` | OCR processes; also the client's concurrency |
| `OCR_MEM_LIMIT` | `8g` | keep at roughly `workers × 3GB` + headroom |
| `API_MEM_LIMIT` | `2g` | |
| `DOLICO_SHIM_TIMEOUT` | `120s` | also sets the `?wait=true` ceiling, +30s |
| `DOLICO_MAX_UPLOAD_BYTES` | `268435456` | 256MB |

## Verifying a deployment

```bash
make verify
```

This runs the repository's full end-to-end sweep against the containers that
are already running, rather than against a server it starts for itself: every
fixture uploaded over HTTP, each returned document validated against
`schema/canonical-v1.json` by a real JSON Schema validator, per-page routing
asserted, the scanned table checked for a 5×3 grid in the right order, the error
paths checked for the right status codes, and re-uploads checked for
idempotency. It is the same checker `make e2e` uses, pointed somewhere else.

By hand, if you want the three-line version:

```bash
curl -fsS http://127.0.0.1:8080/healthz | jq          # shim executable, process up
curl -fsS http://127.0.0.1:8080/v1/engines | jq       # which engines are wired
curl -F file=@testdata/scanned-table.pdf \
     'http://127.0.0.1:8080/v1/documents?wait=true' | jq '.pages[0].blocks[0]'
```

Note that `make test-ocr` will *not* work against this deployment: the OCR
service is reachable from the API container, not from the host, so the live
tests in that target have nothing to connect to. They expect a local
`make ocr`.

`/v1/engines` should list `anydoc`, `pdf-inspector` and `pp-structurev3`. If it
shows `ocr-stub` instead, the API could not reach the OCR service and is
serving placeholder text for scanned pages — which it also says loudly in its
startup log.
