include .env
export

.PHONY: up down migrate rollback status migration goose seed fresh dev serve build

up:            ; docker compose up -d --wait
down:          ; docker compose down
migrate:       ; go run ./cmd/migrate up
rollback:      ; go run ./cmd/migrate down
status:        ; go run ./cmd/migrate status
migration:     ; go run ./cmd/migrate create $(name)
goose:         ; go run ./cmd/migrate $(cmd)

# Vite must be up first: the server picks dev vs built assets at boot via public/hot.
# ponytail: no Go auto-reload, add air when restarting by hand gets old.
dev:
	@trap 'kill 0' EXIT; \
	(cd web && npx vite) & \
	until [ -f public/hot ]; do sleep 0.2; done; \
	go run ./cmd/server

serve:         ; go run ./cmd/server
seed:          ; go run ./cmd/server seed
fresh:         ; go run ./cmd/migrate reset && go run ./cmd/migrate up && go run ./cmd/server seed
build:         ; cd web && npm run build
