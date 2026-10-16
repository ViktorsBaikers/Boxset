# Boxset dev toolchain images: dev, lock, work (plus the host redactor and
# capture validator build stages). Every direct artifact below is pinned by
# exact version and SHA-256/digest; docs/spikes/s5-version-admission.md lists
# the same pins and `scripts/s5-toolchain.sh --admission` cross-checks both.
# No Node and no alternative JS package manager is installed anywhere, and no
# full Chrome archive: every accepted browser proof is headless with no
# channel, which resolves to Playwright's chromium-headless-shell.
# Built from the repository root: `docker build -f deploy/dev/Dockerfile .`
# with the deny-by-default root `.dockerignore`.

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS base
ARG DEBIAN_SNAPSHOT=20261005T000000Z
RUN set -eu; \
    printf '%s\n' \
      'Types: deb' \
      "URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}" \
      'Suites: trixie trixie-updates' \
      'Components: main' \
      'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
      '' \
      'Types: deb' \
      "URIs: http://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT}" \
      'Suites: trixie-security' \
      'Components: main' \
      'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
      > /etc/apt/sources.list.d/debian.sources; \
    printf '%s\n' 'Acquire::Check-Valid-Until "false";' 'Acquire::Retries "3";' \
      > /etc/apt/apt.conf.d/99boxset-snapshot

# fetch: checksum-verified upstream archives, unpacked. Never part of a
# runnable image; only the unzip copyright file is carried forward as licence
# evidence for the build-only apt-fetch row.
FROM base AS fetch
ARG TARGETARCH
RUN test "$TARGETARCH" = amd64 || { echo "dev image admits linux/amd64 only" >&2; exit 1; }
ARG APT_FETCH_PACKAGES="\
    unzip=6.0-29+deb13u1 \
"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ${APT_FETCH_PACKAGES} \
    && rm -rf /var/lib/apt/lists/*
ARG GO_VERSION=1.27.2
ARG GO_SHA256=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
ADD --checksum=sha256:${GO_SHA256} https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz /dl/go.tar.gz
ARG BUN_VERSION=1.4.3
ARG BUN_SHA256=3e730528d89775f03f87f05c06787c4df4099d0a37f3f252e7b829cb23816e18
ADD --checksum=sha256:${BUN_SHA256} https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-x64.zip /dl/bun.zip
ARG GOLANGCI_LINT_VERSION=2.14.0
ARG GOLANGCI_LINT_SHA256=ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab
ADD --checksum=sha256:${GOLANGCI_LINT_SHA256} https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz /dl/golangci-lint.tar.gz
# Playwright's chromium-headless-shell for the admitted playwright-core
# revision (browsers.json chromium-headless-shell revision 1248).
ARG PLAYWRIGHT_CHROMIUM_REVISION=1248
ARG CHROME_HEADLESS_SHELL_VERSION=156.0.8078.4
ARG CHROME_HEADLESS_SHELL_SHA256=c3e50596aef52c58f8fda3c6b3411c33d34c88b177b3ed1c2243359fd3b8ba20
ADD --checksum=sha256:${CHROME_HEADLESS_SHELL_SHA256} https://storage.googleapis.com/chrome-for-testing-public/${CHROME_HEADLESS_SHELL_VERSION}/linux64/chrome-headless-shell-linux64.zip /dl/chrome-headless-shell.zip
RUN set -eu; \
    mkdir -p /out/go /out/bin /out/ms-playwright /out/evidence/apt-fetch; \
    tar -xzf /dl/go.tar.gz -C /out/go --strip-components=1; \
    unzip -q -j /dl/bun.zip bun-linux-x64/bun -d /out/bin; \
    tar -xzf /dl/golangci-lint.tar.gz -C /out/bin --strip-components=1 \
      "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint"; \
    shell="/out/ms-playwright/chromium_headless_shell-${PLAYWRIGHT_CHROMIUM_REVISION}"; \
    mkdir -p "$shell"; \
    unzip -q /dl/chrome-headless-shell.zip -d "$shell"; \
    : > "$shell/INSTALLATION_COMPLETE"; \
    cp /usr/share/doc/unzip/copyright /out/evidence/apt-fetch/unzip.copyright; \
    chmod 0755 /out/bin/bun /out/bin/golangci-lint

# dev: Go, Bun, golangci-lint, C compiler, CA/timezone data, QEMU user
# emulation and Playwright's pinned chromium-headless-shell. Host runner/driver
# and the Docker CLI/socket are never copied in. The proof child is the fixed
# unprivileged user boxset 1000:1000 whatever the host UID. Everything under
# /usr and /etc, the toolchain trees and the input manifest are normalized to
# root without group/other write below.
FROM base AS dev
ARG APT_PACKAGES="\
    ca-certificates=20250419 \
    fonts-liberation=1:2.1.5-3 \
    gcc=4:14.2.0-1 \
    libasound2t64=1.2.14-1+deb13u1 \
    libatk-bridge2.0-0t64=2.56.2-1+deb13u2 \
    libatk1.0-0t64=2.56.2-1+deb13u2 \
    libatspi2.0-0t64=2.56.2-1+deb13u2 \
    libc6-dev=2.41-12+deb13u4 \
    libcairo2=1.18.4-1+b1 \
    libcups2t64=2.4.10-3+deb13u2 \
    libdbus-1-3=1.16.2-2 \
    libdrm2=2.4.124-2 \
    libexpat1=2.8.3-1~deb13u1 \
    libfontconfig1=2.15.0-2.3 \
    libfreetype6=2.13.3+dfsg-1+deb13u1 \
    libgbm1=25.0.7-2+deb13u1 \
    libglib2.0-0t64=2.84.4-3~deb13u5 \
    libnspr4=2:4.36-1 \
    libnss3=2:3.110-1+deb13u4 \
    libpango-1.0-0=1.56.3-1 \
    libudev1=257.13-1~deb13u1 \
    libx11-6=2:1.8.12-1 \
    libxcb1=1.17.0-2+b1 \
    libxcomposite1=1:0.4.6-1 \
    libxdamage1=1:1.1.6-1+b2 \
    libxext6=2:1.3.4-1+b3 \
    libxfixes3=1:6.0.0-2+b4 \
    libxkbcommon0=1.7.0-2 \
    libxrandr2=2:1.5.4-1+b3 \
    qemu-user=1:10.0.13+ds-0+deb13u1 \
    tzdata=2026c-0+deb13u1 \
"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ${APT_PACKAGES} \
    && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /out/go /usr/local/go
COPY --from=fetch /out/bin/ /usr/local/bin/
COPY --from=fetch /out/ms-playwright /ms-playwright
COPY --from=fetch /out/evidence /usr/local/share/boxset/evidence
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin
RUN set -eu; \
    groupadd --gid 1000 boxset; \
    useradd --uid 1000 --gid 1000 --home-dir /home/boxset --create-home --shell /usr/sbin/nologin boxset; \
    mkdir -p /var/cache/go-build /go/pkg/mod /src /usr/local/libexec/boxset /usr/local/share/boxset; \
    GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off GOENV=off CGO_ENABLED=1 go build std; \
    GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off GOENV=off CGO_ENABLED=0 go build std; \
    chown -R root:root /usr/local/go /usr/local/bin /ms-playwright /usr/local/share/boxset /usr/local/libexec/boxset; \
    chown -R boxset:boxset /var/cache/go-build /go /home/boxset; \
    chown root:root /src; \
    chmod 1777 /src; \
    chmod -R go-w /usr/local/go /usr/local/bin /ms-playwright; \
    find /usr /etc -xtype l -delete; \
    find /usr /etc ! -type l -perm /022 -exec chmod go-w {} +

# lock: bootstrap only, never a proof runtime. Generates one ecosystem's lock
# with the admitted tools; `lock-export` publishes exactly that ecosystem's
# files, so a Go export can never rewrite web/bun.lock and vice versa:
#   docker build -f deploy/dev/Dockerfile --target lock-export \
#     --build-arg LOCK_ECOSYSTEM=web --output type=local,dest=web .
#   docker build -f deploy/dev/Dockerfile --target lock-export \
#     --build-arg LOCK_ECOSYSTEM=go --output type=local,dest=. .
FROM dev AS lock
ARG LOCK_ECOSYSTEM
WORKDIR /src
COPY . /src/
RUN set -eu; \
    mkdir -p /out; \
    export HOME=/home/boxset TMPDIR=/tmp GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOTELEMETRY=off; \
    case "$LOCK_ECOSYSTEM" in \
      web) cd /src/web && bun install --lockfile-only --ignore-scripts && cp bun.lock /out/bun.lock ;; \
      go) cd /src && go mod tidy && cp go.mod /out/go.mod && { [ ! -f go.sum ] || cp go.sum /out/go.sum; } ;; \
      *) echo "LOCK_ECOSYSTEM must be web or go" >&2; exit 1 ;; \
    esac
FROM scratch AS lock-export
COPY --from=lock /out/ /

# capture-check: stdlib-only capture validator, built from this module.
FROM dev AS capture-check-build
WORKDIR /src
COPY go.mod /src/
COPY deploy/dev/capture-check/ /src/deploy/dev/capture-check/
RUN GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off GOENV=off \
    CGO_ENABLED=0 go build -trimpath -o /capture-check ./deploy/dev/capture-check

# redact-build / host-redact: the stdlib-only output redactor. It is exported
# to the runner's private temp directory (`--output type=local`) and runs on
# the host, so owner values never enter Docker; it is never installed in a
# proof image.
FROM dev AS redact-build
WORKDIR /src
COPY go.mod /src/
COPY deploy/dev/redact/ /src/deploy/dev/redact/
RUN GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off GOENV=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /redact ./deploy/dev/redact
FROM scratch AS host-redact
COPY --from=redact-build /redact /redact

# work: allowlisted current inputs plus frozen dependencies. Rebuilt from the
# current context on every runner invocation; dependency layers are cached by
# their manifests/locks and never regenerate a lock. /src and the existing
# ancestors of the issue-draft root stay root:root 1777, so the child can
# create its own entries there but cannot replace a root-owned one; everything
# else under /src belongs to the child. `/capture` (1000:1000, 0700) is the
# child's own private browser-capture directory, exported only after the
# container stopped; it is never mounted.
FROM dev AS work
WORKDIR /src
COPY --from=capture-check-build /capture-check /usr/local/libexec/boxset/capture-check
RUN chown root:root /usr/local/libexec/boxset/capture-check \
    && chmod 0555 /usr/local/libexec/boxset/capture-check
COPY web/package.json web/bun.lock /src/web/
RUN cd /src/web && HOME=/home/boxset bun install --frozen-lockfile --ignore-scripts
COPY go.mod go.sum* /src/
RUN cd /src && GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
    GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org go mod download
COPY . /src/
RUN set -eu; \
    find /src -path /src/web/node_modules -prune -o -type f -print0 \
      | LC_ALL=C sort -z | xargs -0 sha256sum > /usr/local/share/boxset/inputs.sha256; \
    chown root:root /usr/local/share/boxset/inputs.sha256; \
    chmod 0644 /usr/local/share/boxset/inputs.sha256; \
    chown -R boxset:boxset /src /go /var/cache/go-build /home/boxset; \
    for d in /src /src/testdata /src/docs /src/docs/spikes /src/docs/spikes/data; do \
      if [ -d "$d" ]; then chown root:root "$d"; chmod 1777 "$d"; fi; \
    done; \
    install -d -o 1000 -g 1000 -m 0700 /capture; \
    find /usr /etc -xtype l -delete; \
    find /usr /etc ! -type l -perm /022 -exec chmod go-w {} +
