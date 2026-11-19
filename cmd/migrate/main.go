package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"nomanqureshi.dev/hpyo-agent/db"
)

const usage = "usage: migrate <up|down|status|reset|redo|version|create NAME>"

func main() {
	if len(os.Args) < 2 {
		log.Fatal(usage)
	}
	cmd, args := os.Args[1], os.Args[2:]

	// create writes a new file to disk; no DB needed.
	if cmd == "create" {
		if len(args) < 1 {
			log.Fatal("usage: migrate create NAME")
		}
		if err := goose.Create(nil, "db/migrations", args[0], "sql"); err != nil {
			log.Fatal(err)
		}
		return
	}

	conn, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal(err)
	}
	if err := goose.RunContext(context.Background(), cmd, conn, "migrations", args...); err != nil {
		log.Fatal(err)
	}
}
