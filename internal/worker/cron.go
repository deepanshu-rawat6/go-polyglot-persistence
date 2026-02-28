package worker

import (
	"context"
	"log/slog"
	"time"

	"go-polyglot-persistence/internal/database"

	"github.com/robfig/cron/v3"
)

// StartCronJobs registers the materialized view refresh on the given schedule
// and starts the scheduler. Returns an error if the schedule string is invalid
// so that main() can fail fast with a clear message instead of a buried panic.
//
// The returned *cron.Cron must be stopped on shutdown:
//
//	c, err := StartCronJobs(db, cfg.MVRefreshSchedule)
//	defer c.Stop()  // waits for any running job to finish before returning
func StartCronJobs(db *database.DB, schedule string) (*cron.Cron, error) {
	c := cron.New()

	_, err := c.AddFunc(schedule, func() {
		slog.Info("mv refresh started", "component", "cron")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		if err := db.RefreshMaterializedView(ctx); err != nil {
			slog.Error("mv refresh failed", "component", "cron", "error", err)
		} else {
			slog.Info("mv refresh done", "component", "cron")
		}
	})
	if err != nil {
		return nil, err
	}

	c.Start()
	slog.Info("cron scheduler started", "component", "cron", "schedule", schedule)
	return c, nil
}
