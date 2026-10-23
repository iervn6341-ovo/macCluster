# Build the static Linux binary first (GOOS=linux GOARCH=<node arch> go build -trimpath -o maccluster-agent-linux ./cmd/maccluster-agent);
# use the repository root as build context.
FROM scratch
COPY maccluster-agent-linux /maccluster-agent
USER 65532:65532
ENTRYPOINT ["/maccluster-agent", "egress-proxy"]
