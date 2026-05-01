FROM golang:1.26.2-bookworm

ENV TERM=xterm

RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl ca-certificates openssh-client \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://claude.ai/install.sh | bash

RUN go install github.com/goreleaser/goreleaser/v2@v2.15.3 \
    && go clean -cache -modcache

ENV PATH="/root/.local/bin:${PATH}"

WORKDIR /workspaces/codehamr
