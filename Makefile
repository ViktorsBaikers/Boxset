# Each recipe is one command that runs the same without make; the comment above it names it. They
# call only docker, docker compose, git and scripts/dev-run.sh: the host needs nothing else.
.PHONY: dev test lint build

# docker compose -f compose.dev.yaml up --build --renew-anon-volumes
dev:
	docker compose -f compose.dev.yaml up --build --renew-anon-volumes

# scripts/dev-run.sh -- sh -euc 'bun --cwd=web run build; env CGO_ENABLED=1 go test -race -p 2 -count=1 ./...; exec bun --cwd=web run test'
test:
	scripts/dev-run.sh -- sh -euc 'bun --cwd=web run build; env CGO_ENABLED=1 go test -race -p 2 -count=1 ./...; exec bun --cwd=web run test'

# scripts/dev-run.sh -- sh -euc 'go vet ./...; golangci-lint run; exec bun --cwd=web run check'
lint:
	scripts/dev-run.sh -- sh -euc 'go vet ./...; golangci-lint run; exec bun --cwd=web run check'

# docker build -f deploy/dev/Dockerfile --target binary --output type=local,dest=bin .
build:
	docker build -f deploy/dev/Dockerfile --target binary --output type=local,dest=bin .
