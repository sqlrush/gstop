package gsbench

import (
	"context"
	"database/sql"
	"math/rand"
	"sync/atomic"
	"time"
)

func TPStatements(schema string) []string {
	return []string{
		"SELECT balance FROM " + schema + ".accounts WHERE id=$1",
		"UPDATE " + schema + ".accounts SET balance=balance+1,updated_at=current_timestamp WHERE id=$1",
		"INSERT INTO " + schema + ".orders(id,customer_id,status,amount,created_at) VALUES($1,$2,0,$3,current_timestamp)",
	}
}

type TPScenario struct{ *cpuWorkloadScenario }

func NewTPScenario() *TPScenario {
	return &TPScenario{cpuWorkloadScenario: &cpuWorkloadScenario{name: "tp_cpu", build: buildTPWorkload}}
}

var orderSequence atomic.Int64

func buildTPWorkload(ctx context.Context, rt *Runtime, name string) *sqlWorkload {
	orderSequence.CompareAndSwap(0, time.Now().UnixNano())
	statements := TPStatements(rt.Config.Data.Schema)
	return newSQLWorkload(ctx, rt, name, rt.Config.Safety.MaxWorkers, func(ctx context.Context, conn *sql.Conn, workerID int) error {
		id := rand.Int63n(10000) + 1
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var balance float64
		if err := tx.QueryRowContext(ctx, statements[0], id).Scan(&balance); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, statements[1], id); err != nil {
			return err
		}
		if workerID%10 == 0 {
			orderID := orderSequence.Add(1)
			if _, err := tx.ExecContext(ctx, statements[2], orderID, id, balance); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
