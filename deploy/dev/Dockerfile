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
ARG DEBIAN_SNAPSHOT=20261018T000000Z
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
ARG SQLC_VERSION=1.31.1
ARG SQLC_SHA256=497ae4fcdfa64c5b0c311ffe4c2bd991e43991e82e5367792ed78bc2dca27354
ADD --checksum=sha256:${SQLC_SHA256} https://github.com/sqlc-dev/sqlc/releases/download/v${SQLC_VERSION}/sqlc_${SQLC_VERSION}_linux_amd64.tar.gz /dl/sqlc.tar.gz
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
    tar -xzf /dl/sqlc.tar.gz -C /out/bin sqlc; \
    shell="/out/ms-playwright/chromium_headless_shell-${PLAYWRIGHT_CHROMIUM_REVISION}"; \
    mkdir -p "$shell"; \
    unzip -q /dl/chrome-headless-shell.zip -d "$shell"; \
    : > "$shell/INSTALLATION_COMPLETE"; \
    cp /usr/share/doc/unzip/copyright /out/evidence/apt-fetch/unzip.copyright; \
    chmod 0755 /out/bin/bun /out/bin/golangci-lint /out/bin/sqlc

# dev: Go, Bun, golangci-lint, sqlc, C compiler, CA/timezone data, QEMU user
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
    libexpat1=2.9.0-1~deb13u1 \
    libfontconfig1=2.15.0-2.3 \
    libfreetype6=2.13.3+dfsg-1+deb13u1 \
    libgbm1=25.0.7-2+deb13u1 \
    libglib2.0-0t64=2.84.4-3~deb13u5 \
    liblzma5=5.8.1-1+deb13u2 \
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
# else under /src belongs to the child, except the benchmark records the
# context may carry under docs/spikes/data (a run's before/during/after.json and
# samples/samples.jsonl): those files and every directory below data are
# root:root read-only (0444/0555), so the child can overwrite, unlink or replace
# none of them. `/capture` (1000:1000, 0700) is the
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
# The offline -mod=readonly consumers (go list, go mod tidy -diff, go mod verify and the go.sum
# regeneration of scripts/s5-toolchain.sh --admission) find this image-only cache at the runner's
# exact module locations (GOPATH/GOMODCACHE of the child environment). It is filled from a
# go.sum-less scratch copy of go.mod under GOSUMDB=sum.golang.org: `go mod download all` caches
# the whole module graph, test-only dependencies of dependencies included, and the checksum
# database records the regeneration reads. /src/go.sum is never written.
RUN set -eu; \
    mkdir /tmp/dl; \
    cp /src/go.mod /tmp/dl/go.mod; \
    cd /tmp/dl; \
    GOPATH=/go GOMODCACHE=/go/pkg/mod GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOENV=off GOFLAGS=-mod=mod \
    GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org go mod download all; \
    rm -rf /tmp/dl
COPY . /src/
RUN set -eu; \
    find /src -path /src/web/node_modules -prune -o -type f -print0 \
      | LC_ALL=C sort -z | xargs -0 sha256sum > /usr/local/share/boxset/inputs.sha256; \
    chown root:root /usr/local/share/boxset/inputs.sha256; \
    chmod 0644 /usr/local/share/boxset/inputs.sha256; \
    chown -R boxset:boxset /src /go /var/cache/go-build /home/boxset; \
    for d in /src /src/docs /src/docs/spikes /src/docs/spikes/data; do \
      if [ -d "$d" ]; then chown root:root "$d"; chmod 1777 "$d"; fi; \
    done; \
    if [ -d /src/docs/spikes/data ]; then \
      find /src/docs/spikes/data -mindepth 1 -exec chown -h root:root {} +; \
      find /src/docs/spikes/data -mindepth 1 -type d -exec chmod 0555 {} +; \
      find /src/docs/spikes/data -mindepth 1 -type f -exec chmod 0444 {} +; \
    fi; \
    install -d -o 1000 -g 1000 -m 0700 /capture; \
    find /usr /etc -xtype l -delete; \
    find /usr /etc ! -type l -perm /022 -exec chmod go-w {} +
# The generated Paraglide messages module is what `svelte-kit sync` + `svelte-check` type-check
# against, and the runner's standalone `check` has no vite build before it: compile it here, from
# the frozen plugin, offline, as the child's own user.
USER boxset
RUN cd /src/web && HOME=/home/boxset bun --bun paraglide-js compile --project ./project.inlang --outdir ./.svelte-kit/paraglide
USER root

# live: the Compose dev image (`compose.dev.yaml`, `make dev`). FROM work, so the web shell is built
# here, in the stage, by the committed `build` script (adapter-static `rmSync`s its whole `pages`
# directory, so this never happens in the owner tree); Compose copies that shell up into an anonymous
# `internal/server/dist` volume. The uid-1000 targets /config, /var/cache/bun and web/.svelte-kit
# exist here, beside the already uid-1000 web/node_modules, /go/pkg/mod and /var/cache/go-build,
# so fresh named volumes copy up that ownership. /usr/local/share/boxset/deps is the root-owned
# read-only dependency seed `deploy/dev/watch.ts` reads: copies of the frozen web/node_modules and Go
# download cache (a file proxy for GOPROXY) plus the manifest stamps web.sha256 (bun version, then
# sha256sum of web/package.json and web/bun.lock) and go.sha256 (sha256sum of go.mod and go.sum);
# web.sha256 is also the node_modules stamp, written last.
FROM work AS live
USER boxset
RUN cd /src && HOME=/home/boxset bun --cwd=web run build
USER root
RUN set -eu; \
    seed=/usr/local/share/boxset/deps; \
    mkdir -p "$seed/web" "$seed/go/cache" /config /var/cache/bun; \
    cp -a /src/web/node_modules "$seed/web/node_modules"; \
    cp -a /go/pkg/mod/cache/download "$seed/go/cache/download"; \
    { bun --version; cd /src/web && sha256sum package.json bun.lock; } > "$seed/web.sha256"; \
    { cd /src && sha256sum go.mod go.sum; } > "$seed/go.sha256"; \
    chown -R root:root "$seed"; \
    chmod -R go-w,a+rX "$seed"; \
    cp "$seed/web.sha256" /src/web/node_modules/.boxset-deps.sha256; \
    chown 1000:1000 /src/web/node_modules/.boxset-deps.sha256 /config /var/cache/bun; \
    chmod 0700 /config
# Source entries are directory-backed links, while generated paths stay real volume targets.
# The dependency alias is outside the readonly checkout bind for native config imports.
RUN set -eu; \
    mkdir -p /checkout/source; \
    ln -s /src/web/node_modules /checkout/node_modules; \
    for entry in .prettierignore bun.lock e2e eslint.config.js messages package.json \
      playwright.config.ts prettier.config.js project.inlang scripts src tsconfig.json vite.config.ts; do \
      rm -rf "/src/web/$entry"; \
      ln -s "/checkout/source/web/$entry" "/src/web/$entry"; \
    done
WORKDIR /src

# binary-build / binary: `make build`. The shell is built in the stage, the production command is
# built CGO_ENABLED=0 -trimpath and must answer GET / with the rewritten shell before the scratch
# stage `binary` exports only /boxset (the lock-export/host-redact local-export pattern).
FROM work AS binary-build
USER boxset
RUN set -eu; \
    cd /src; \
    HOME=/home/boxset bun --cwd=web run build; \
    test -s internal/server/dist/200.html || { echo "binary-build: the web build produced no 200.html" >&2; exit 1; }; \
    export GOPATH=/go GOMODCACHE=/go/pkg/mod GOCACHE=/var/cache/go-build GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off GOENV=off; \
    CGO_ENABLED=0 go build -trimpath -o /tmp/boxset ./cmd/boxset; \
    go version -m /tmp/boxset | grep -q 'CGO_ENABLED=0' || { echo "binary-build: the binary was not built with CGO_ENABLED=0" >&2; exit 1; }; \
    mkdir /tmp/data; \
    env -i BOXSET_DATA_DIR=/tmp/data BOXSET_PORT=18484 /tmp/boxset serve & pid=$!; \
    trap 'kill "$pid" 2>/dev/null || true' EXIT; \
    n=0; \
    until bun -e 'const r = await fetch("http://127.0.0.1:18484/ping"); process.exit(r.status === 200 && r.headers.get("x-boxset-state") === "ready" ? 0 : 1)' 2>/dev/null; do \
      n=$((n + 1)); [ "$n" -lt 100 ] || { echo "binary-build: the binary never became ready" >&2; exit 1; }; sleep 0.3; \
    done; \
    bun -e 'const r = await fetch("http://127.0.0.1:18484/"); const t = await r.text(); process.exit(r.status === 200 && t.includes("<html") && !t.includes("__boxset_base__") ? 0 : 1)' \
      || { echo "binary-build: GET / is not the rewritten shell" >&2; exit 1; }; \
    bun -e 'import assert from "node:assert/strict"; import { readdirSync, readFileSync } from "node:fs"; const dir="internal/server/dist/_app/immutable/assets"; const pkg="web/node_modules/@fontsource-variable/archivo"; for (const [suffix, source] of [[".woff2","files/archivo-latin-standard-normal.woff2"],[".txt","LICENSE"]]) { const names=readdirSync(dir).filter(n=>n.endsWith(suffix)); assert.equal(names.length,1); const r=await fetch("http://127.0.0.1:18484/_app/immutable/assets/"+names[0],{headers:{"Accept-Encoding":"identity"}}); assert.equal(r.status,200); assert.deepEqual(Buffer.from(await r.arrayBuffer()),readFileSync(pkg+"/"+source)); }'; \
    kill -TERM "$pid"; \
    wait "$pid"; \
    mkdir /tmp/out; \
    cp /tmp/boxset /tmp/out/boxset
FROM scratch AS binary
COPY --from=binary-build /tmp/out/boxset /boxset
