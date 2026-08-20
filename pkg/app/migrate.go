package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// runMigrate 實作 `/app migrate up|down [N]|version`（docs/07 §1.8 第 5 點）。
func runMigrate(log *slog.Logger, base config.Base, service string, args []string) int {
	if service == "" {
		log.Error("this service does not own a database; migrate subcommand is unavailable")
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: app migrate up|down [N]|version")
		return 2
	}
	src, err := migrations.Source(service)
	if err != nil {
		log.Error("migration source", "err", err)
		return 1
	}
	url := base.EffectiveMigrateURL()
	if url == "" {
		log.Error("PG_DATABASE_URL (or PG_MIGRATE_DATABASE_URL) is required")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "up":
		if err := pgdb.Migrate(ctx, url, service, src); err != nil {
			log.Error("migrate up failed", "service", service, "err", err)
			return 1
		}
		log.Info("migrate up done", "service", service)
	case "down":
		steps := 1
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, "migrate down: N must be a positive integer")
				return 2
			}
			steps = n
		}
		if err := pgdb.MigrateDown(ctx, url, service, src, steps); err != nil {
			log.Error("migrate down failed", "service", service, "steps", steps, "err", err)
			return 1
		}
		log.Info("migrate down done", "service", service, "steps", steps)
	case "version":
		v, dirty, err := pgdb.MigrateVersion(ctx, url, service, src)
		if err != nil {
			log.Error("migrate version failed", "service", service, "err", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "%d %v\n", v, dirty)
		if dirty {
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: app migrate up|down [N]|version")
		return 2
	}
	return 0
}
