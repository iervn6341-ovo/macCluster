#!/bin/zsh
# Builds the Service controller image for linux/amd64 and linux/arm64 (docs/service-controller-plan.md).
#
#   scripts/build-controller-image.sh --image REGISTRY/maccluster-network-controller:VERSION [--push]
#   scripts/build-controller-image.sh --image NAME:TAG --platform linux/arm64 --load     # one platform, into local Docker
#
# Without --push or --load it only builds, writing an OCI archive to dist/controller/image.tar (nothing leaves the
# Mac). With --push it pushes and prints the image's digest: deploy by digest (name@sha256:...), never by tag.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="" PLATFORMS="linux/amd64,linux/arm64" PUSH=0 LOAD=0
while (( $# )); do
  case "$1" in
    --image) IMAGE="$2"; shift 2 ;;
    --platform) PLATFORMS="$2"; shift 2 ;;
    --push) PUSH=1; shift ;;
    --load) LOAD=1; shift ;;
    *) print -u2 "unknown argument: $1"; exit 2 ;;
  esac
done
[[ -n "$IMAGE" ]] || { print -u2 "usage: $0 --image NAME:TAG [--platform P] [--push|--load]"; exit 2; }
(( PUSH && LOAD )) && { print -u2 "--push and --load are exclusive"; exit 2; }
if (( LOAD )) && [[ "$PLATFORMS" == *,* ]]; then print -u2 "--load takes a single --platform"; exit 2; fi

GO="${MACCLUSTER_GO:-$ROOT/.tools/go/bin/go}"
if [[ ! -x "$GO" ]]; then GO="$(command -v go)"; fi
export GOPATH="$ROOT/.tools/gopath" GOMODCACHE="$ROOT/.tools/gomod" GOCACHE="$ROOT/.tools/gocache"
CTX="$ROOT/dist/controller/context"
mkdir -p "$CTX"
cd "$ROOT"
for platform in ${(s:,:)PLATFORMS}; do
  arch="${platform#linux/}"
  [[ "$platform" == linux/* && ( "$arch" == amd64 || "$arch" == arm64 ) ]] || { print -u2 "unsupported platform $platform (amd64 and arm64 only)"; exit 2; }
  print "building maccluster-agent for $platform"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$GO" build -trimpath -ldflags="-s -w" -o "$CTX/maccluster-agent-linux-$arch" ./cmd/maccluster-agent
done
cp "$ROOT/deploy/network/Dockerfile" "$CTX/Dockerfile"

args=(buildx build --platform "$PLATFORMS" --file "$CTX/Dockerfile" --tag "$IMAGE" --provenance=false)
if (( PUSH )); then
  META="$ROOT/dist/controller/metadata.json"
  args+=(--push --metadata-file "$META")
elif (( LOAD )); then
  args+=(--load)
else
  args+=(--output "type=oci,dest=$ROOT/dist/controller/image.tar")
fi
docker "${args[@]}" "$CTX"
if (( PUSH )); then
  DIGEST="$(python3 -c "import json,sys;print(json.load(open('$ROOT/dist/controller/metadata.json'))['containerimage.digest'])")"
  print "pushed ${IMAGE%%:*}@$DIGEST"
  print "deploy with: maccluster-agent network-controller-manifests --image ${IMAGE%%:*}@$DIGEST --namespaces NS1,NS2"
elif (( LOAD )); then
  print "loaded $IMAGE into the local Docker"
else
  print "wrote dist/controller/image.tar (OCI archive, platforms: $PLATFORMS); not pushed"
fi
