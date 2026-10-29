package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/anywaye/press/internal/migrate"
	"github.com/anywaye/press/migrations"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	dsn := os.Getenv("DATABASE_URL")
	applied, err := migrate.Run(context.Background(), dsn, migrations.FS, slog.Default())
	if err != nil {
		slog.Error("press-migrate: failed", "error", err)
		os.Exit(1)
	}
	slog.Info("press-migrate: applied", "count", len(applied), "files", applied)
}
