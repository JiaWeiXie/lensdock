package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IdemRecord is a stored idempotent response.
type IdemRecord struct {
	Fingerprint string
	Response    []byte
	CreatedAt   time.Time
}

// IdemLookup returns the record stored for (scope, key), or nil when none.
func IdemLookup(ctx context.Context, q Querier, scope, key string) (*IdemRecord, error) {
	var (
		rec IdemRecord
		ms  int64
	)
	err := q.QueryRowContext(ctx, `SELECT fingerprint, response, created_at FROM idempotency WHERE scope = ? AND key = ?`, scope, key).
		Scan(&rec.Fingerprint, &rec.Response, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.CreatedAt = time.UnixMilli(ms).UTC()
	return &rec, nil
}

// IdemStore records the response of an idempotent operation. It is meant to
// run in the same transaction as the state change it describes.
func IdemStore(ctx context.Context, q Querier, scope, key, fingerprint string, response []byte, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO idempotency (scope, key, fingerprint, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		scope, key, fingerprint, response, now.UnixMilli())
	return err
}

// IdemPurge deletes records created before the cutoff.
func IdemPurge(ctx context.Context, q Querier, before time.Time) (int64, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM idempotency WHERE created_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
