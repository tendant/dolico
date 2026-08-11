# Deploying dolico as an internal service

Two containers on one host: the API server and the OCR tier. The vision tier is
not included — see *Turning on the vision tier* below.

```bash
docker compose -f deploy/docker-compose.yml up -d --build
curl -F file=@testdata/mixed.pdf 'http://127.0.0.1:8080/v1/documents?wait=true'
```

The first start is slow and *reports itself unhealthy while it is*: PaddleOCR
downloads a few hundred megabytes of models and loads them once per worker, and
`/healthz` returns 503 until that finishes. The API deliberately refuses to
start until the OCR service is healthy, so `docker compose up` will sit there
for a few minutes the first time and start quickly on every restart after,
because the models live in a volume.

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

**On Apple Silicon you also have to turn oneDNN off:**

```bash
DOLICO_PADDLE_MKLDNN=False docker compose -f deploy/docker-compose.yml up -d
```

Paddle's oneDNN backend is x86 code. Emulated, inference dies inside the model
runner with `ConvertPirAttribute2RuntimeAttribute not support`, which reaches
the API as a bare `500` from `/v1/extract` and says nothing about the cause. The
API handles it correctly — the page comes back with `ocr_failed` in its reasons
and the error in `trace.engines` — but every scanned page is empty. Leave the
flag alone on amd64, where oneDNN works and is the faster path.

The API image has no such constraint and builds natively for the host.

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

## Upgrading

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

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
| `DOLICO_PORT` | `8080` | host port, bound to `127.0.0.1` |
| `DOLICO_OCR_WORKERS` | `2` | OCR processes; also the client's concurrency |
| `OCR_MEM_LIMIT` | `8g` | keep at roughly `workers × 3GB` + headroom |
| `API_MEM_LIMIT` | `2g` | |
| `DOLICO_SHIM_TIMEOUT` | `120s` | also sets the `?wait=true` ceiling, +30s |
| `DOLICO_MAX_UPLOAD_BYTES` | `268435456` | 256MB |

## Verifying a deployment

```bash
make deploy-verify
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
