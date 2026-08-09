#!/usr/bin/env bash
# Score extraction against ground truth on a cold cache.
#
# A fresh data directory every run, because the document-level cache
# short-circuits a document that has already been processed -- which is correct
# for the service and useless for a benchmark, since the second run reports
# six milliseconds and measures nothing.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${DOLICO_BENCH_PORT:-8098}"
BASE="http://127.0.0.1:${PORT}"
DATA_DIR="$(mktemp -d -t dolico-bench-XXXXXX)"
LOG="${DATA_DIR}/server.log"

if [[ ! -x ./bin/dolico ]]; then
    echo "bin/dolico not found -- run 'make build' first" >&2
    exit 1
fi

cleanup() {
    if [[ -n "${SERVER_PID:-}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
        kill "${SERVER_PID}" 2>/dev/null || true
        wait "${SERVER_PID}" 2>/dev/null || true
    fi
    rm -rf "${DATA_DIR}"
}
trap cleanup EXIT

if [[ -n "${DOLICO_OCR_URL:-}" ]]; then
    if ! curl -fsS "${DOLICO_OCR_URL}/healthz" >/dev/null 2>&1; then
        echo "DOLICO_OCR_URL=${DOLICO_OCR_URL} is not healthy; start it with 'make ocr'" >&2
        exit 1
    fi
    tier=$(curl -fsS "${DOLICO_OCR_URL}/v1/version" | sed -n 's/.*"tier":"\([^"]*\)".*/\1/p')
    echo "OCR tier: ${DOLICO_OCR_URL} (${tier:-unknown})"
else
    echo "OCR tier: stub -- scanned pages will score as total failures."
    echo "          Start one with 'make ocr' and set DOLICO_OCR_URL for real numbers."
fi

DOLICO_ADDR="127.0.0.1:${PORT}" DOLICO_DATA_DIR="${DATA_DIR}" ./bin/dolico >"${LOG}" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 200); do
    curl -fsS "${BASE}/healthz" >/dev/null 2>&1 && break
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
        echo "server exited during startup:" >&2
        cat "${LOG}" >&2
        exit 1
    fi
    sleep 0.1
done

./scripts/bench.py --base "${BASE}" "$@"
