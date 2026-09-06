#!/usr/bin/env bash
set -euo pipefail

# Emp3r0r Web API end-to-end file ops verifier
# Covers: ls, upload, download, rm, mkdir, stat, cp, mv
# Requires: curl, base64, md5sum/sha256sum

BASE_URL="https://localhost:9443/api"
TOKEN_FILE="${HOME}/.emp3r0r/web_token.txt"
AGENT_TAG="${AGENT_TAG:-}"
WORKDIR_BASE="/tmp/emp3r0r-webtest"
TEST_DIR="${WORKDIR_BASE}/$(date +%s)"
ARTIFACT_DIR="${ARTIFACT_DIR:-/tmp/emp3r0r-webtest-artifacts}"
CURL_OPTS=( -sS -k -H "Authorization: Bearer $(cat "$TOKEN_FILE")" -H 'Content-Type: application/json' )

echo "[i] Using BASE_URL=$BASE_URL"
echo "[i] Token file: $TOKEN_FILE"
mkdir -p "$ARTIFACT_DIR"

json_escape() {
  # escape backslash and double quotes for JSON
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "[!] Missing dependency: $1"; exit 1; }
}

require curl
require base64
if ! command -v md5sum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
  echo "[!] Missing md5sum/sha256sum"
  exit 1
fi

# Resolve agent tag if not provided
if [[ -z "$AGENT_TAG" ]]; then
  echo "[i] Discovering active agents..."
  AGENTS_JSON=$(curl "${CURL_OPTS[@]}" "$BASE_URL/agents")
  # naive extract first Tag value (keeps escapes like \\)
  AGENT_TAG=$(printf '%s' "$AGENTS_JSON" | grep -o '"Tag":"[^"]*"' | head -n1 | cut -d':' -f2- | tr -d '"')
  if [[ -z "$AGENT_TAG" ]]; then
    echo "[!] No agents found. Connect an agent and retry."
    exit 1
  fi
  echo "[i] Using AgentTag=$AGENT_TAG"
fi

AGENT_JSON_TAG=$(json_escape "$AGENT_TAG")

pass() { echo -e "[PASS] $*"; }
fail() { echo -e "[FAIL] $*"; exit 1; }

# Health
curl -sS -k "$BASE_URL/health" >/dev/null || fail "health check failed"
pass "health"

# 1) mkdir
BODY=$(printf '{"AgentTag":"%s","Path":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$TEST_DIR")")
HTTP_CODE=$(curl -o /dev/null -w '%{http_code}' "${CURL_OPTS[@]}" -X POST "$BASE_URL/mkdir" --data "$BODY")
[[ "$HTTP_CODE" == "200" ]] || fail "mkdir http $HTTP_CODE"
pass "mkdir $TEST_DIR"

# 2) ls
BODY=$(printf '{"AgentTag":"%s","Path":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$TEST_DIR")")
RESP=$(curl "${CURL_OPTS[@]}" -X POST "$BASE_URL/ls" --data "$BODY") || fail "ls request failed"
[[ "$RESP" == [* ]] || echo "[warn] ls returned non-array, raw: $RESP"
pass "ls $TEST_DIR"

# 3) upload small text file
SMALL_TXT="$ARTIFACT_DIR/small.txt"
printf 'hello-%s' "$(date +%s)" >"$SMALL_TXT"
B64_SMALL=$(base64 -w 0 "$SMALL_TXT" 2>/dev/null || base64 "$SMALL_TXT" | tr -d '\n')
DST_SMALL="$TEST_DIR/small.txt"
BODY=$(printf '{"AgentTag":"%s","FilePath":"%s","Content":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$DST_SMALL")" "$B64_SMALL")
RESP=$(curl "${CURL_OPTS[@]}" -X POST "$BASE_URL/upload" --data "$BODY") || fail "upload small"
pass "upload small.txt"

# 4) stat
BODY=$(printf '{"AgentTag":"%s","Path":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$DST_SMALL")")
RESP=$(curl "${CURL_OPTS[@]}" -X POST "$BASE_URL/stat" --data "$BODY") || fail "stat failed"
printf '%s' "$RESP" | grep -q '"size"' || echo "[warn] stat response: $RESP"
pass "stat $DST_SMALL"

# 5) cp -> copy small.txt to copy.txt
DST_COPY="$TEST_DIR/copy.txt"
BODY=$(printf '{"AgentTag":"%s","Src":"%s","Dst":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$DST_SMALL")" "$(json_escape "$DST_COPY")")
HTTP_CODE=$(curl -o /dev/null -w '%{http_code}' "${CURL_OPTS[@]}" -X POST "$BASE_URL/cp" --data "$BODY")
[[ "$HTTP_CODE" == "200" ]] || fail "cp http $HTTP_CODE"
pass "cp -> $DST_COPY"

# 6) mv -> rename copy.txt to moved.txt
DST_MOVED="$TEST_DIR/moved.txt"
BODY=$(printf '{"AgentTag":"%s","Src":"%s","Dst":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$DST_COPY")" "$(json_escape "$DST_MOVED")")
HTTP_CODE=$(curl -o /dev/null -w '%{http_code}' "${CURL_OPTS[@]}" -X POST "$BASE_URL/mv" --data "$BODY")
[[ "$HTTP_CODE" == "200" ]] || fail "mv http $HTTP_CODE"
pass "mv -> $DST_MOVED"

# 7) upload big binary (2MB)
BIG_LOCAL="$ARTIFACT_DIR/big.bin"
python3 - <<'PY' "$BIG_LOCAL" 2>/dev/null || true
import os, sys
path = sys.argv[1]
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path,'wb') as f: f.write(os.urandom(2*1024*1024))
print(path)
PY
if [[ ! -s "$BIG_LOCAL" ]]; then
  dd if=/dev/urandom of="$BIG_LOCAL" bs=1M count=2 status=none
fi
MD5_BIN=""
if command -v md5sum >/dev/null 2>&1; then MD5_BIN=md5sum; else MD5_BIN=sha256sum; fi
LOCAL_HASH=$($MD5_BIN "$BIG_LOCAL" | awk '{print $1}')
B64_BIG=$(base64 -w 0 "$BIG_LOCAL" 2>/dev/null || base64 "$BIG_LOCAL" | tr -d '\n')
DST_BIG="$TEST_DIR/big.bin"
BODY=$(printf '{"AgentTag":"%s","FilePath":"%s","Content":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$DST_BIG")" "$B64_BIG")
RESP=$(curl "${CURL_OPTS[@]}" -X POST "$BASE_URL/upload" --data "$BODY") || fail "upload big"
pass "upload big.bin (2MB)"

# 8) download big to verify FTP streaming
OUT_DL="$ARTIFACT_DIR/big.dl.bin"
# raw fetch to get blob
curl -sS -k -H "Authorization: Bearer $(cat "$TOKEN_FILE")" -X POST "$BASE_URL/download" --data "$(printf '{"AgentTag":"%s","FilePath":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$DST_BIG")")" -o "$OUT_DL" || fail "download big"
REMOTE_HASH=$($MD5_BIN "$OUT_DL" | awk '{print $1}')
if [[ "$LOCAL_HASH" != "$REMOTE_HASH" ]]; then
  fail "hash mismatch: local=$LOCAL_HASH remote=$REMOTE_HASH"
fi
pass "download big.bin (hash match)"

# 9) rm files
for p in "$DST_SMALL" "$DST_MOVED" "$DST_BIG"; do
  BODY=$(printf '{"AgentTag":"%s","Path":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$p")")
  HTTP_CODE=$(curl -o /dev/null -w '%{http_code}' "${CURL_OPTS[@]}" -X POST "$BASE_URL/rm" --data "$BODY")
  [[ "$HTTP_CODE" == "200" ]] || fail "rm $p -> http $HTTP_CODE"
  pass "rm $p"
done

# 10) rm dir
BODY=$(printf '{"AgentTag":"%s","Path":"%s"}' "$AGENT_JSON_TAG" "$(json_escape "$TEST_DIR")")
HTTP_CODE=$(curl -o /dev/null -w '%{http_code}' "${CURL_OPTS[@]}" -X POST "$BASE_URL/rm" --data "$BODY")
[[ "$HTTP_CODE" == "200" ]] || echo "[warn] rm dir returned $HTTP_CODE (non-empty or not supported)"
pass "cleanup"

echo "\nAll checks passed."