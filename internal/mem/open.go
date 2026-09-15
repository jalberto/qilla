package mem

import (
	"database/sql"
	"fmt"

	"github.com/jalberto/qilla/internal/config"
)

// Open builds the store for cfg on the shared database.
func Open(cfg *config.Config, db *sql.DB) (*Store, error) {
	var b Backend
	var err error
	switch cfg.Memory.Backend {
	case "sqlite":
		b, err = NewSQLite(db)
	default:
		b, err = NewEngram(cfg.Memory.EngramURL)
	}
	if err != nil {
		return nil, fmt.Errorf("memory backend %s: %w", cfg.Memory.Backend, err)
	}
	return New(b, db, Options{SharedProject: cfg.Memory.SharedProject, MaxChars: cfg.Memory.MaxChars, SaidTTLDays: cfg.Memory.SaidTTLDays,
		ExpireUnusedDays: cfg.Memory.ExpireUnusedDays, PromoteSupport: cfg.Memory.PromoteSupport, PromoteDays: cfg.Memory.PromoteDays})
}
