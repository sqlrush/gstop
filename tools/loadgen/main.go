// Command loadgen drives a continuous, concurrent workload against a
// GaussDB/openGauss instance so gstop can be observed against live traffic. It
// runs three worker pools:
//
//   - TP  : many short transactions (UPDATE + SELECT on a keyed row) → high
//     TPS/QPS and a churn of active sessions.
//   - AP  : a few long analytical queries (a self-join aggregate) → slow SQL and
//     long-running active sessions.
//   - Lock: several transactions contending on a couple of hot rows while
//     holding the lock briefly → lock waits (BLK H/W/H&W) and a block tree.
//
// It runs until interrupted (Ctrl-C / SIGTERM). This is a development/test tool,
// not part of the gstop deliverable.
package main

import (
	"database/sql"
	"flag"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "gitcode.com/opengauss/openGauss-connector-go-pq"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("GSTOP_LOADGEN_DSN"), "connection string (or set GSTOP_LOADGEN_DSN)")
	tp := flag.Int("tp", 50, "number of TP workers")
	ap := flag.Int("ap", 2, "number of slow-AP workers")
	lock := flag.Int("lock", 5, "number of lock-contention workers")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("database connection string is required via -dsn or GSTOP_LOADGEN_DSN")
	}

	log.Printf("loadgen starting: tp=%d ap=%d lock=%d", *tp, *ap, *lock)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	launch(&wg, *tp, func(id int) { tpWorker(*dsn, stop, id) })
	launch(&wg, *ap, func(id int) { apWorker(*dsn, stop, id) })
	launch(&wg, *lock, func(id int) { lockWorker(*dsn, stop, id) })

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("loadgen stopping...")
	close(stop)
	wg.Wait()
	log.Println("loadgen stopped")
}

func launch(wg *sync.WaitGroup, n int, fn func(id int)) {
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) { defer wg.Done(); fn(id) }(i)
	}
}

// open returns a single-connection pool so each worker maps to one DB session.
func open(dsn string) *sql.DB {
	db, err := sql.Open("opengauss", dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	return db
}

func stopped(stop chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// tpWorker runs a tight loop of small transactions against random rows.
func tpWorker(dsn string, stop chan struct{}, id int) {
	db := open(dsn)
	defer db.Close()
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*7))
	for !stopped(stop) {
		_, _ = db.Exec("UPDATE bench.accounts SET balance = balance + 1, updated = now() WHERE id = $1", r.Intn(10000)+1)
		var bal int64
		_ = db.QueryRow("SELECT balance FROM bench.accounts WHERE id = $1", r.Intn(10000)+1).Scan(&bal)
		time.Sleep(time.Duration(5+r.Intn(25)) * time.Millisecond)
	}
}

// apWorker repeatedly runs a slow analytical self-join aggregate.
func apWorker(dsn string, stop chan struct{}, id int) {
	db := open(dsn)
	defer db.Close()
	const q = "SELECT f1.dim1, count(*) c, sum(f1.amount) s, avg(f2.amount) a " +
		"FROM bench.facts f1 JOIN bench.facts f2 ON f1.dim2 = f2.dim2 GROUP BY f1.dim1 ORDER BY c DESC LIMIT 10"
	for !stopped(stop) {
		rows, err := db.Query(q)
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// lockWorker holds a row lock on one of a few hot rows for a few seconds inside a
// transaction, forcing waits and a block tree among the pool.
func lockWorker(dsn string, stop chan struct{}, id int) {
	db := open(dsn)
	defer db.Close()
	hot := (id % 2) + 1 // contend on rows 1 and 2
	for !stopped(stop) {
		tx, err := db.Begin()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		_, _ = tx.Exec("UPDATE bench.accounts SET balance = balance + 1, updated = now() WHERE id = $1", hot)
		_, _ = tx.Exec("SELECT pg_sleep(3)")
		_ = tx.Commit()
		time.Sleep(200 * time.Millisecond)
	}
}
