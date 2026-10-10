# mail-bridge: bridge between mail clients (IMAP, SMTP) and the edge in Convex.
#
#   docker build -t mail-bridge .            Production image (last stage)
#   docker build --target dev -t mail-bridge-dev .      Development image with a test edge (dev.sh)
#   docker build --target probe -t mail-bridge-edge .   Probe against a given edge (test/against-edge.sh)
#
# All runtime stages: static binary, distroless, no shell, non-root. The last stage is the one
# for production.

FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/mail-bridge ./cmd/mail-bridge \
 && go build -trimpath -ldflags="-s -w" -o /out/mail-bridge-dev ./cmd/devstack

# For local development only: bridge and test edge in one process.
FROM gcr.io/distroless/static-debian12:nonroot AS dev
COPY --from=build /out/mail-bridge-dev /mail-bridge-dev
USER nonroot:nonroot
EXPOSE 1143 1465 8080
ENTRYPOINT ["/mail-bridge-dev"]

# For the probe against a given edge (test/against-edge.sh) only: the bridge, built with
# -tags localedge (talks to the edge over http even beyond this machine, see
# internal/edge/plainhttp_on.go), and the probe program. Never for production.
FROM build AS probe-build
RUN go build -trimpath -tags localedge -ldflags="-s -w" -o /out/mail-bridge-localedge ./cmd/mail-bridge \
 && go build -trimpath -ldflags="-s -w" -o /out/edgeprobe ./cmd/edgeprobe

FROM gcr.io/distroless/static-debian12:nonroot AS probe
COPY --from=probe-build /out/mail-bridge-localedge /mail-bridge
COPY --from=probe-build /out/edgeprobe /edgeprobe
USER nonroot:nonroot
EXPOSE 1143 1465 8080
ENTRYPOINT ["/mail-bridge"]

# Production. Contains neither the test edge nor the probe program and is built without localedge.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/mail-bridge /mail-bridge
USER nonroot:nonroot
EXPOSE 1143 1465 8080
ENTRYPOINT ["/mail-bridge"]
