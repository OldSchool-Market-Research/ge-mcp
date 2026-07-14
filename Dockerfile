# ge-mcp: one static Go binary on distroless. This image is both runnable
# standalone (stdio MCP server) and a binary-carrier: ge-orchestrator's image
# COPY --from's /ge-mcp so the agent can spawn it in-pod.

# ---- build ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/ge-mcp .

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ge-mcp /ge-mcp
USER nonroot:nonroot
ENTRYPOINT ["/ge-mcp"]
