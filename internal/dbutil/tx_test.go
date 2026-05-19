package dbutil_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

func TestRunInTx_CommitsOnSuccess(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("dbutil_smoke_%d", uniqueID())
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id int)`, tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	})

	err = dbutil.RunInTx(ctx, pool, func(tx dbutil.DBTX) error {
		_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id) VALUES (1)`, tableName))
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tableName)).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestRunInTx_RollsBackOnError(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("dbutil_smoke_rb_%d", uniqueID())
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id int)`, tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName))
	})

	wantErr := errSentinel
	err = dbutil.RunInTx(ctx, pool, func(tx dbutil.DBTX) error {
		_, _ = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id) VALUES (1)`, tableName))
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("RunInTx err = %v, want %v", err, wantErr)
	}

	var count int
	_ = pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tableName)).Scan(&count)
	if count != 0 {
		t.Errorf("count = %d, want 0 (rolled back)", count)
	}
}

var errSentinel = &sentinelErr{}

type sentinelErr struct{}

func (*sentinelErr) Error() string { return "sentinel" }

var idCounter atomic.Int64

func uniqueID() int64 {
	return idCounter.Add(1)
}
