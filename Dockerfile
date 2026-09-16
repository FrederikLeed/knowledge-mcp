# syntax=docker/dockerfile:1
# Knowledge MCP container image.
#
#   docker build -t knowledge-mcp --build-arg VERSION=$(scripts/build-number local) .
#   docker run -d --name knowledge-mcp -v knowledge-data:/data -p 127.0.0.1:8765:8765 knowledge-mcp
#
# The default command listens on all container interfaces with
# --allow-non-loopback. The server has no authentication: publish the port on
# loopback only, or attach the container to a private Docker network.

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=container
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/lkarlslund/knowledge-mcp/internal/mcpserver.Version=${VERSION}" \
      -o /out/knowledge-mcp .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S knowledge && adduser -S -G knowledge -h /data knowledge \
 && mkdir -p /data /usr/share/knowledge-mcp && chown knowledge:knowledge /data
COPY --from=build /out/knowledge-mcp /usr/local/bin/knowledge-mcp
COPY contrib/webpages /usr/share/knowledge-mcp/webpages
USER knowledge
VOLUME ["/data"]
EXPOSE 8765
ENV KNOWLEDGE_MCP_SERVER=http://127.0.0.1:8765/mcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8765/healthz || exit 1
ENTRYPOINT ["knowledge-mcp"]
CMD ["serve", "--listen", "0.0.0.0:8765", "--allow-non-loopback", "--data-dir", "/data"]
