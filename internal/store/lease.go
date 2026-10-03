package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Lease fences the single-host service deployment. It uses a dedicated
// connection, so pool_max_conns=1 remains usable by commands and activities.
// PostgreSQL releases the advisory lock when the process/connection disappears.
type Lease struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

func (s *Store) AcquireLease(ctx context.Context) (*Lease, error) {
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	var acquired bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,0))", "knotra.engine."+s.EngineID).Scan(&acquired)
	if err != nil || !acquired {
		conn.Close(ctx)
		if err == nil {
			err = fmt.Errorf("another engine process is already running against this database")
		}
		return nil, err
	}
	return &Lease{conn: conn}, nil
}

func (l *Lease) Check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn.Ping(ctx)
}

func (l *Lease) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn.Close(ctx)
}
