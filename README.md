# Hypo Agent

An agentic loop for hypothesis research and validation.

**Working on the code?** Read [AGENTS.md](AGENTS.md) first: architecture, durability rules, the agent state machine, and conventions.

Go + Postgres (pgvector) backend, Inertia (gonertia) + React + shadcn/ui frontend.

## Setup

```bash
cp .env.example .env
# in .env: set NEBIUS_TOKEN_FACTORY, and OBSCURA_CDP_TOKEN to `openssl rand -hex 32`
cd web && npm install && cd ..
make up
make migrate
make seed       # test@example.com / password
make dev        # http://localhost:8080
```

## Commands

**Database (Docker)**
```bash
make up                 # start Postgres + pgvector, Temporal + UI, Obscura (browser), SearXNG (search)
make down               # stop them
docker compose down -v  # stop it and wipe the data
```

**Migrations** (goose, files in `db/migrations`)
```bash
make migration name=create_users   # new migration file
make migrate                       # run pending
make rollback                      # undo the last one
make status                        # applied vs pending
make goose cmd=reset               # undo everything
make goose cmd=redo                # undo + re-run the last one
make goose cmd=version             # current DB version
make seed                          # run seeders, safe to repeat  (≈ db:seed)
make fresh                         # reset + migrate + seed       (≈ migrate:fresh --seed)
```

**App**
```bash
make dev                # Vite (127.0.0.1:5174) + Go server with hot reload (air) → http://localhost:8080
make build              # build the frontend into public/build
make serve              # Go server only, using the built files
```

**Frontend**
```bash
cd web && npx shadcn@latest add dialog   # add a shadcn component
cd web && npm run typecheck
cd web && npm run lint
cd web && npm run format                # kit style: 4 spaces, single quotes
```

## How it fits together

- `cmd/server/main.go` renders pages: `app.Render(w, r, "agent", inertia.Props{...})`.
- `"agent"` maps to `web/src/pages/agent.tsx`. Layouts are picked by page name
  in `web/src/app.tsx` (`auth/*` → auth layout, `settings/*` → agent shell + settings layout, else the agent shell in `layouts/agent-layout.tsx`).
- `web/app.html` is the root template.
- In dev, Vite writes `public/hot` so the server loads assets from the Vite dev server.
- Go hot reload: [air](https://github.com/air-verse/air), config in `.air.toml`, pinned in `go.mod`
  as a tool (`go tool air`). Rebuilds on `.go` and `web/app.html` changes.
- Vite runs on `127.0.0.1:5174`, not 5173: DDEV's router holds `127.0.0.1:5173`.

## Starter kit

The frontend is the [Laravel React starter kit](https://github.com/laravel/react-starter-kit)
(MIT), ported to the Go backend.

| Route | Page |
|---|---|
| `/` | `pages/welcome.tsx` |
| `/login`, `/register` | `pages/auth/*` (guest only) |
| `/agent` | `pages/agent.tsx` (auth) |
| `/settings/profile`, `/settings/security`, `/settings/appearance` | `pages/settings/*` (auth) |

- Routes and handlers: `cmd/server/main.go`; sessions + password hashing: `cmd/server/auth.go`.
- Shared props on every page: `auth.user`, `name`, `sidebarOpen` (from the `sidebar_state` cookie).
- `web/src/routes/` stands in for Laravel Wayfinder (`agent()`, `store.form()`, ...).
  Add a route there when you add one in `main.go`.
- Seeders: `cmd/server/seed.go` (keep inserts idempotent, e.g. `ON CONFLICT DO NOTHING`).
- Validation errors: re-render the page via `s.invalid(...)`; the kit's `<Form>` picks up `errors`.
- Inertia v3 client on gonertia (v2 HTML): `app.tsx` passes the initial page from `data-page`.
- Not ported (need mail or extra backend): forgot/reset password, email verification,
  two-factor auth, passkeys, password confirmation.
