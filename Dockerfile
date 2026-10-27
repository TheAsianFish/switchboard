# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/switchboard ./cmd/switchboard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switchboard /usr/local/bin/switchboard
COPY deploy/switchboard.yaml /etc/switchboard/switchboard.yaml
ENV SWITCHBOARD_CONFIG=/etc/switchboard/switchboard.yaml \
    SWITCHBOARD_LISTEN=0.0.0.0:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/switchboard"]
CMD ["serve"]
