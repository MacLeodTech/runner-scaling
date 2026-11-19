# syntax=docker/dockerfile:1
# CI runner image family: the pinned runner image plus the tools CI jobs would otherwise download or compile on every
# run. Built by images/build.sh from a versions file; never by hand, never by the pools' own CI.
#
#   base    runner image + jq, python3 (venv, pip), build tools, gosec, gitleaks, govulncheck
#   lite    base + Go, Node (two lines), JDKs, Maven, Rust. Nothing here needs a Docker daemon.
#   sysbox  lite + Playwright with Chromium and its system libraries
#
# Layout rule: tools live under /opt/ci and are symlinked into /usr/local/bin. NEVER under /opt/hostedtoolcache:
# the guards bind-mount a per-slot directory there, which would hide anything baked into the image. Go and Node are
# also laid out as an actions toolcache under /opt/ci/toolcache, so a guard that sets RUNNER_TOOL_CACHE to it lets
# actions/setup-go and setup-node find them without downloading.
ARG BASE_IMAGE

# ---- build-only stage: fetch, verify, compile. Nothing from here but the finished files reaches the images. ----
FROM ${BASE_IMAGE} AS fetch
ARG GO_VERSION GO_SHA256 NODE_VERSION NODE_SHA256 NODE_OLD_VERSION NODE_OLD_SHA256 MAVEN_VERSION MAVEN_SHA512 \
    RUSTUP_VERSION RUSTUP_INIT_SHA256 GOSEC_VERSION GOSEC_SHA256 GITLEAKS_VERSION GITLEAKS_SHA256 GOVULNCHECK_VERSION
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN mkdir -p /out/opt/ci/toolcache /out/opt/ci/bin /dl
WORKDIR /dl
# get <url> <sha256|sha512> <file>: download and refuse to continue on a mismatch.
RUN cat > /usr/local/bin/get <<'SH'
#!/bin/bash
set -euo pipefail
url=$1 sum=$2 file=$3
curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$file" "$url"
case ${#sum} in
  64) echo "$sum  $file" | sha256sum -c - ;;
  128) echo "$sum  $file" | sha512sum -c - ;;
  *) echo "get: unusable checksum for $url" >&2; exit 1 ;;
esac
SH
RUN chmod +x /usr/local/bin/get

# Go, laid out as an actions toolcache entry (go/<version>/x64 plus the .complete marker).
RUN if [ -n "${GO_VERSION:-}" ]; then \
      get "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" "$GO_SHA256" go.tgz; \
      d=/out/opt/ci/toolcache/go/${GO_VERSION}/x64; mkdir -p "$d"; tar -xzf go.tgz -C "$d" --strip-components=1; touch "$d.complete"; \
    fi
# Node, same layout.
RUN for p in "${NODE_VERSION:-}:${NODE_SHA256:-}" "${NODE_OLD_VERSION:-}:${NODE_OLD_SHA256:-}"; do \
      v=${p%%:*}; s=${p#*:}; [ -n "$v" ] || continue; \
      get "https://nodejs.org/dist/v$v/node-v$v-linux-x64.tar.xz" "$s" node.txz; \
      d=/out/opt/ci/toolcache/node/$v/x64; mkdir -p "$d"; tar -xJf node.txz -C "$d" --strip-components=1; touch "$d.complete"; \
    done
# Maven
RUN if [ -n "${MAVEN_VERSION:-}" ]; then \
      get "https://repo.maven.apache.org/maven2/org/apache/maven/apache-maven/$MAVEN_VERSION/apache-maven-$MAVEN_VERSION-bin.tar.gz" "$MAVEN_SHA512" mvn.tgz; \
      mkdir -p /out/opt/ci/maven; tar -xzf mvn.tgz -C /out/opt/ci/maven --strip-components=1; \
    fi
# gosec, gitleaks: release tarballs
RUN if [ -n "${GOSEC_VERSION:-}" ]; then \
      get "https://github.com/securego/gosec/releases/download/v$GOSEC_VERSION/gosec_${GOSEC_VERSION}_linux_amd64.tar.gz" "$GOSEC_SHA256" gosec.tgz; \
      tar -xzf gosec.tgz -C /out/opt/ci/bin gosec; \
    fi
RUN if [ -n "${GITLEAKS_VERSION:-}" ]; then \
      get "https://github.com/gitleaks/gitleaks/releases/download/v$GITLEAKS_VERSION/gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz" "$GITLEAKS_SHA256" gl.tgz; \
      tar -xzf gl.tgz -C /out/opt/ci/bin gitleaks; \
    fi
# govulncheck: compiled with the Go just verified; module integrity comes from Go's checksum database (GONOSUMDB unset).
RUN if [ -n "${GOVULNCHECK_VERSION:-}" ] && [ -n "${GO_VERSION:-}" ]; then \
      GOBIN=/out/opt/ci/bin GOPATH=/tmp/gopath GOFLAGS=-trimpath CGO_ENABLED=0 \
        /out/opt/ci/toolcache/go/${GO_VERSION}/x64/bin/go install "golang.org/x/vuln/cmd/govulncheck@v${GOVULNCHECK_VERSION}"; \
    fi
RUN if [ -n "${RUSTUP_VERSION:-}" ]; then \
      get "https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-gnu/rustup-init" "$RUSTUP_INIT_SHA256" rustup-init; \
      chmod +x rustup-init; mv rustup-init /out/opt/ci/bin/rustup-init; \
    fi

# ---- base ----
FROM ${BASE_IMAGE} AS base
ARG GO_VERSION NODE_VERSION
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN apt-get update && apt-get install -y --no-install-recommends jq python3-venv python3-pip build-essential pkg-config \
    && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /out/opt/ci/bin/gosec /out/opt/ci/bin/gitleaks /out/opt/ci/bin/govulncheck /opt/ci/bin/
RUN for t in gosec gitleaks govulncheck; do [ -x /opt/ci/bin/$t ] && ln -sf /opt/ci/bin/$t /usr/local/bin/$t || true; done
LABEL org.opencontainers.image.title="runner-scaling CI runner (base)"

# ---- lite ----
FROM base AS lite
ARG JDK_MAJORS RUST_VERSION GO_VERSION NODE_VERSION NODE_OLD_VERSION MAVEN_VERSION
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN if [ -n "${JDK_MAJORS:-}" ]; then \
      apt-get update && for m in $JDK_MAJORS; do apt-get install -y --no-install-recommends "openjdk-$m-jdk-headless"; done \
      && rm -rf /var/lib/apt/lists/*; \
      first=${JDK_MAJORS%% *}; update-java-alternatives -s "java-1.$first.0-openjdk-amd64" 2>/dev/null || true; \
    fi
COPY --from=fetch /out/opt/ci/toolcache /opt/ci/toolcache
COPY --from=fetch /out/opt/ci/maven /opt/ci/maven
COPY --from=fetch /out/opt/ci/bin/rustup-init /opt/ci/bin/
ENV RUSTUP_HOME=/opt/ci/rustup CARGO_HOME=/opt/ci/cargo
RUN if [ -n "${GO_VERSION:-}" ]; then ln -sf /opt/ci/toolcache/go/${GO_VERSION}/x64/bin/go /opt/ci/toolcache/go/${GO_VERSION}/x64/bin/gofmt /usr/local/bin/; fi; \
    if [ -n "${NODE_VERSION:-}" ]; then for b in node npm npx corepack; do ln -sf /opt/ci/toolcache/node/${NODE_VERSION}/x64/bin/$b /usr/local/bin/$b; done; fi; \
    if [ -n "${MAVEN_VERSION:-}" ]; then ln -sf /opt/ci/maven/bin/mvn /usr/local/bin/mvn; fi; \
    if [ -n "${RUST_VERSION:-}" ]; then \
      /opt/ci/bin/rustup-init -y --profile minimal --default-toolchain "$RUST_VERSION" --no-modify-path \
      && for b in cargo rustc rustup rustdoc; do ln -sf /opt/ci/cargo/bin/$b /usr/local/bin/$b; done; \
    fi; \
    rm -f /opt/ci/bin/rustup-init
LABEL org.opencontainers.image.title="runner-scaling CI runner (lite)"

# ---- sysbox ----
FROM lite AS sysbox
ARG PLAYWRIGHT_VERSION
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ci/ms-playwright
RUN if [ -n "${PLAYWRIGHT_VERSION:-}" ]; then \
      npx --yes "playwright@${PLAYWRIGHT_VERSION}" install --with-deps chromium \
      && rm -rf /var/lib/apt/lists/* /root/.npm; \
    fi
LABEL org.opencontainers.image.title="runner-scaling CI runner (sysbox)"
