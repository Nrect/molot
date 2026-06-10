# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src

# Module download as its own cached layer: re-runs only when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/monolith ./cmd/monolith

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/monolith /monolith
EXPOSE 8080
ENTRYPOINT ["/monolith"]
