#!/usr/bin/env bash
# Fallback proto generator used by `make proto` when `buf` is not installed.
#
# Produces exactly the same layout as buf.gen.yaml:
#   api/proto/pg/<service>/v1/foo.proto  ->  api/gen/go/pg/<service>/v1/foo.pb.go (+ foo_grpc.pb.go)
#
# Requirements: protoc on PATH, protoc-gen-go + protoc-gen-go-grpc on PATH (make tools puts them in ./bin).
# go_package in every .proto must be:
#   option go_package = "github.com/tenghongzou/paymentgateway/api/gen/go/pg/<service>/v1;<service>v1";
# With paths=source_relative the output directory mirrors the proto path, which matches that import path.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROTO_DIR="api/proto"
OUT_DIR="api/gen/go"
export PATH="$ROOT/bin:$PATH"

for tool in protoc protoc-gen-go protoc-gen-go-grpc; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: $tool not found on PATH (run 'make tools' and install protoc)" >&2
    exit 1
  fi
done

mkdir -p "$OUT_DIR"

# Collect all versioned proto files: api/proto/pg/**/v1/*.proto (also v2, v3... for future versions)
PROTO_DIRS="$(find "$PROTO_DIR/pg" -type f -path '*/v[0-9]*/*.proto' 2>/dev/null | xargs -n1 dirname 2>/dev/null | sort -u || true)"

if [ -z "$PROTO_DIRS" ]; then
  echo "no .proto files found under $PROTO_DIR/pg/**/v*/ - nothing to generate"
  exit 0
fi

echo ">> protoc: generating into $OUT_DIR"
# One protoc invocation per package directory so that cross-file imports within a package resolve cleanly.
for dir in $PROTO_DIRS; do
  echo "   - $dir"
  # shellcheck disable=SC2046
  protoc \
    -I "$PROTO_DIR" \
    --go_out="$OUT_DIR" --go_opt=paths=source_relative \
    --go-grpc_out="$OUT_DIR" --go-grpc_opt=paths=source_relative \
    --go-grpc_opt=require_unimplemented_servers=true \
    $(find "$dir" -maxdepth 1 -name '*.proto' | sort)
done

echo ">> done"
