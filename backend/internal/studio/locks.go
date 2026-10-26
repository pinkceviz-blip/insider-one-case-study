package studio

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

// A session-scoped advisory lock survives transaction commits. The exact pooled
// connection remains pinned until unlock. If unlock fails, destroy the connection
// rather than returning a potentially locked session to the pool.
func (s *Store) lockOperation(ctx context.Context, key string) (*pgxpool.Conn, func(), error) {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,0))", key).Scan(&locked); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = conn.Conn().Close(cleanup)
		conn.Release()
		return nil, nil, err
	}
	if !locked {
		conn.Release()
		return nil, nil, &DeferredError{After: 500 * time.Millisecond}
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(cleanup, "SELECT pg_advisory_unlock(hashtextextended($1,0))", key).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Conn().Close(cleanup)
		}
		conn.Release()
	}
	return conn, release, nil
}

func rollbackTx(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
