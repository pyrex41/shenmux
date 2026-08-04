FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/shenmux ./cmd/shenmux

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libzmq5 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/shenmux /usr/local/bin/shenmux
RUN useradd --system --home-dir /var/lib/shenmux --create-home shenmux
USER shenmux
ENTRYPOINT ["/usr/local/bin/shenmux"]
