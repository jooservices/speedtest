package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/shared/config"
)

// openCenterDatabase is the one way every command reaches the database: it loads .env and the configuration,
// runs check (if any) on the configuration before any connection is made and connects
// to the database the environment names. The caller closes the pool.
func openCenterDatabase(ctx context.Context, check func(config.CenterConfig) error) (*pgxpool.Pool, config.CenterConfig, error) {
	if err := loadDotEnv(); err != nil {
		return nil, config.CenterConfig{}, err
	}
	cfg, err := config.LoadCenter()
	if err != nil {
		return nil, cfg, fmt.Errorf("invalid configuration: %w", err)
	}
	if check != nil {
		if err := check(cfg); err != nil {
			return nil, cfg, err
		}
	}
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return nil, cfg, err
	}
	return pool, cfg, nil
}

func loadDotEnv() error {
	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot load .env")
	}
	return nil
}

func containsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}
