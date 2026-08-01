package gsbench

import (
	"context"
	"database/sql"
	"math/rand"
	"strconv"
	"sync/atomic"
	"time"
)

func TPStatements(schema string, id, orderID int64, balance float64) []string {
	idText := strconv.FormatInt(id, 10)
	distKeyText := strconv.FormatInt(id+1, 10)
	orderText := strconv.FormatInt(orderID, 10)
	balanceText := strconv.FormatFloat(balance, 'f', 2, 64)
	return []string{
		"SELECT balance FROM " + schema + ".accounts WHERE dist_key=" + distKeyText + " AND id=" + idText,
		"UPDATE " + schema + ".accounts SET balance=balance+1,updated_at=current_timestamp WHERE dist_key=" + distKeyText + " AND id=" + idText,
		"INSERT INTO " + schema + ".orders(dist_key,id,customer_id,status,amount,created_at) VALUES(" +
			distKeyText + "," + orderText + "," + idText + ",0," + balanceText + ",current_timestamp)",
	}
}

type TPScenario struct{ *cpuWorkloadScenario }

func NewTPScenario() *TPScenario {
	return &TPScenario{cpuWorkloadScenario: &cpuWorkloadScenario{
		code: 101, name: "tp_cpu", build: buildTPWorkload,
	}}
}

var orderSequence atomic.Int64

func buildTPWorkload(ctx context.Context, rt *Runtime, name string) *sqlWorkload {
	orderSequence.CompareAndSwap(0, time.Now().UnixNano())
	return newSQLWorkload(ctx, rt, name, rt.Config.Safety.MaxWorkers, func(ctx context.Context, conn *sql.Conn, workerID int) error {
		// Every supported dataset has at least 10,000 customers. Keeping the
		// runtime account ID below that boundary makes the generator's
		// mod(id, customers)+1 distribution key exactly id+1.
		id := rand.Int63n(9_999) + 1
		statements := TPStatements(rt.Config.Data.Schema, id, 0, 0)
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var balance float64
		if err := tx.QueryRowContext(ctx, statements[0]).Scan(&balance); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, statements[1]); err != nil {
			return err
		}
		if workerID%10 == 0 {
			orderID := orderSequence.Add(1)
			insertSQL := TPStatements(rt.Config.Data.Schema, id, orderID, balance)[2]
			if _, err := tx.ExecContext(ctx, insertSQL); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
