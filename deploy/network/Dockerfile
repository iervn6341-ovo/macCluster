# Multi-architecture image of the cluster-side Service controller (the cluster has amd64 and arm64 Linux nodes).
# Build with scripts/build-controller-image.sh, which first builds the static Linux binaries this file copies:
#   dist/controller/context/maccluster-agent-linux-amd64
#   dist/controller/context/maccluster-agent-linux-arm64
# and uses that directory as the build context.
FROM scratch
ARG TARGETARCH
COPY maccluster-agent-linux-${TARGETARCH} /maccluster-agent
USER 65532:65532
ENTRYPOINT ["/maccluster-agent", "network-controller"]
