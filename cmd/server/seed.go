package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seed fills the dev database (≈ Laravel's DatabaseSeeder). Keep it idempotent:
// `make seed` can run any number of times.
func seed(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
		INSERT INTO users (name, email, password_hash) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO NOTHING`,
		"Test User", "test@example.com", hashPassword("password"))
	return err
}
