FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/muxd ./cmd/muxd

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates fish zsh \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/muxd /usr/local/bin/muxd
COPY deploy/shenmux-entrypoint /usr/local/bin/shenmux-entrypoint
RUN chmod 0755 /usr/local/bin/shenmux-entrypoint

ENV SHENMUX_SESSION=work \
    SHENMUX_CONTROL=tcp://0.0.0.0:5555 \
    SHENMUX_DATA=tcp://0.0.0.0:5556 \
    SHENMUX_SHELL=/bin/zsh \
    SHENMUX_SHELL_ARGS=-il \
    TERM=xterm-256color

ENTRYPOINT ["/usr/local/bin/shenmux-entrypoint"]
