#!/usr/bin/env bash
# End-to-end check: start a server on a scratch data directory, run every
# fixture through the HTTP API, validate the results against the canonical
# schema, then tear it all down.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${DOLICO_E2E_PORT:-8099}"
BASE="http://127.0.0.1:${PORT}"
DATA_DIR="$(mktemp -d -t dolico-e2e-XXXXXX)"
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
    if [[ "${DOLICO_E2E_KEEP:-}" == "1" ]]; then
        echo "data directory kept at ${DATA_DIR}"
    else
        rm -rf "${DATA_DIR}"
    fi
}
trap cleanup EXIT

if [[ -n "${DOLICO_OCR_URL:-}" ]]; then
    if ! curl -fsS "${DOLICO_OCR_URL}/healthz" >/dev/null 2>&1; then
        echo "DOLICO_OCR_URL is set to ${DOLICO_OCR_URL} but nothing is healthy there." >&2
        echo "Start the OCR service with 'make ocr', or unset DOLICO_OCR_URL to use the stub." >&2
        exit 1
    fi
    echo "OCR tier: ${DOLICO_OCR_URL}"
else
    echo "OCR tier: stub (set DOLICO_OCR_URL for real OCR)"
fi

echo "Starting dolico on ${BASE} (data: ${DATA_DIR})"
DOLICO_ADDR="127.0.0.1:${PORT}" DOLICO_DATA_DIR="${DATA_DIR}" ./bin/dolico >"${LOG}" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
    if curl -fsS "${BASE}/healthz" >/dev/null 2>&1; then
        break
    fi
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
        echo "server exited during startup:" >&2
        cat "${LOG}" >&2
        exit 1
    fi
    sleep 0.1
done

if ! curl -fsS "${BASE}/healthz" >/dev/null 2>&1; then
    echo "server did not become healthy:" >&2
    cat "${LOG}" >&2
    exit 1
fi

if ./scripts/e2e_check.py "${BASE}"; then
    exit 0
fi

echo
echo "--- server log ---" >&2
tail -50 "${LOG}" >&2
exit 1
