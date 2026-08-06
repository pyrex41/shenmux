FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/muxd ./cmd/muxd
# The controller/agent deployment examples need the shenmux binary, not just
# the legacy muxd daemon. Without this the manifests in deploy/kubernetes
# reference an image that cannot run them.
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/shenmux ./cmd/shenmux
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/muxctl ./cmd/muxctl

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl fish git zsh \
    && rm -rf /var/lib/apt/lists/*

# A session opened in a browser is the product surface, so the shell inside the
# container should not be a bare root prompt. starship gives the same prompt
# people get locally, and detects arch itself so this works on arm64 and amd64.
RUN curl -fsSL https://starship.rs/install.sh | sh -s -- --yes \
    && printf 'eval "$(starship init zsh)"\n' >> /root/.zshrc \
    && printf 'eval "$(starship init bash)"\n' >> /root/.bashrc

COPY --from=build /out/muxd /usr/local/bin/muxd
COPY --from=build /out/shenmux /usr/local/bin/shenmux
COPY --from=build /out/muxctl /usr/local/bin/muxctl
COPY deploy/shenmux-entrypoint /usr/local/bin/shenmux-entrypoint
RUN chmod 0755 /usr/local/bin/shenmux-entrypoint

ENV SHENMUX_SESSION=work \
    SHENMUX_CONTROL=tcp://0.0.0.0:5555 \
    SHENMUX_DATA=tcp://0.0.0.0:5556 \
    SHENMUX_SHELL=/bin/zsh \
    SHENMUX_SHELL_ARGS=-il \
    TERM=xterm-256color

ENTRYPOINT ["/usr/local/bin/shenmux-entrypoint"]
