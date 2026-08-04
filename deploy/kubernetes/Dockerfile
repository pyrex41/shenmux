FROM golang:1.23-bookworm AS build
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends libzmq3-dev && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/shenmux ./cmd/shenmux
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/muxd ./cmd/muxd
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/muxctl ./cmd/muxctl

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libzmq5 bash bash-completion curl git && \
    curl -fsSL https://starship.rs/install.sh | sh -s -- -y -b /usr/local/bin && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/shenmux /usr/local/bin/shenmux
COPY --from=build /out/muxd /usr/local/bin/muxd
COPY --from=build /out/muxctl /usr/local/bin/muxctl
RUN useradd --system --home-dir /var/lib/shenmux --create-home shenmux
RUN printf '%s\n' \
  'if [ -f /usr/share/bash-completion/bash_completion ]; then . /usr/share/bash-completion/bash_completion; fi' \
  'if command -v starship >/dev/null 2>&1; then eval "$(starship init bash)"; fi' \
  'export EDITOR=${EDITOR:-vi}' \
  'alias ll="ls -alF"' \
  'alias la="ls -A"' \
  'alias l="ls -CF"' \
  > /etc/profile.d/shenmux.sh
RUN printf '%s\n' \
  'if [ -f /etc/profile.d/shenmux.sh ]; then . /etc/profile.d/shenmux.sh; fi' \
  > /etc/bash.bashrc
USER shenmux
ENTRYPOINT ["/usr/local/bin/shenmux"]
