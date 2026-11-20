include .env
export

.PHONY: up down migrate rollback status migration goose seed fresh dev serve worker build test smoke

up:            ; docker compose up -d --wait
down:          ; docker compose down
migrate:       ; go run ./cmd/migrate up
rollback:      ; go run ./cmd/migrate down
status:        ; go run ./cmd/migrate status
migration:     ; go run ./cmd/migrate create $(name)
goose:         ; go run ./cmd/migrate $(cmd)

# Vite + the agent worker + the server, each hot-reloading. Needs `make up` (Temporal).
dev:
	@trap 'kill 0' EXIT; \
	(cd web && npx vite) & \
	go tool air -c .air.worker.toml & \
	until [ -f public/hot ]; do sleep 0.2; done; \
	go tool air

serve:         ; go run ./cmd/server
worker:        ; go run ./cmd/worker
seed:          ; go run ./cmd/server seed
fresh:         ; go run ./cmd/migrate reset && go run ./cmd/migrate up && go run ./cmd/server seed
build:         ; cd web && npm run build
test:          ; go test -race ./...
smoke:         ; go test -count=1 -v -run Smoke ./internal/model   # real Nebius call
