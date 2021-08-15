package db

import (
	"context"
	"database/sql"
	"fmt"
	"hash/maphash"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/brunotm/statement"
	"github.com/brunotm/statement/scan"
)

type Logger func(message, id string, err error, d time.Duration, query string)

func noopLogger(message, id string, err error, d time.Duration, query string) {}

type Config struct {
	Log      Logger
	ReadOpt  sql.IsolationLevel
	WriteOpt sql.IsolationLevel
}

// DB is a wrapped *sql.DB
type DB struct {
	db       *sql.DB
	log      Logger
	readOpt  *sql.TxOptions
	writeOpt *sql.TxOptions
}

// New creates a new database from an existing *sql.DB.
func New(db *sql.DB, config Config) (d *DB, err error) {
	d = &DB{}
	d.db = db

	d.log = noopLogger
	if config.Log != nil {
		d.log = config.Log
	}

	d.readOpt = &sql.TxOptions{Isolation: config.ReadOpt, ReadOnly: true}
	d.writeOpt = &sql.TxOptions{Isolation: config.WriteOpt, ReadOnly: false}

	return d, nil
}

// Tx creates a database transaction with the provided options.
func (d *DB) Tx(ctx context.Context, tid string, opts *sql.TxOptions) (tx *Tx, err error) {
	t, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}

	if tid == "" {
		tid = strconv.FormatInt(time.Now().UnixNano(), 32)
	}

	return &Tx{
		tid:   tid,
		log:   d.log,
		tx:    t,
		ctx:   ctx,
		cache: map[uint64]reflect.Value{},
	}, nil

}

// Read creates a read-only transaction with the default DB isolation level.
func (d *DB) Read(ctx context.Context, tid string) (tx *Tx, err error) {
	return d.Tx(ctx, tid, d.readOpt)
}

// Update creates a read-write transaction with the default DB isolation level.
func (d *DB) Update(ctx context.Context, tid string) (tx *Tx, err error) {
	return d.Tx(ctx, tid, d.writeOpt)
}

// Tx represents a database transaction
type Tx struct {
	mu    sync.Mutex
	tid   string
	log   Logger
	done  bool
	tx    *sql.Tx
	ctx   context.Context
	hash  maphash.Hash
	cache map[uint64]reflect.Value
}

// Exec executes a query that doesn't return rows.
func (t *Tx) Exec(stmt statement.Statement) (r sql.Result, err error) {
	start := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	query, err := stmt.String()
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}

	r, err = t.tx.ExecContext(t.ctx, query)

	t.log("db.tx.exec", t.tid, err, time.Since(start), query)
	return r, err
}

// Query executes a query that returns rows.
func (t *Tx) Query(dst interface{}, stmt statement.Statement) (err error) {
	start := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	query, err := stmt.String()
	if err != nil {
		t.log("db.tx.query.build", t.tid, err, time.Since(start), fmt.Sprintf("%#v", stmt))
		return err
	}

	if _, err = t.hash.WriteString(query); err != nil {
		return err
	}

	key := t.hash.Sum64()
	t.hash.Reset()

	if r, ok := t.cache[key]; ok {
		reflect.ValueOf(dst).Elem().Set(r)
		t.log("db.tx.query.cached", t.tid, nil, time.Since(start), query)
		return nil
	}

	r, err := t.tx.QueryContext(t.ctx, query)
	if err != nil {
		return err
	}

	if _, err = scan.Load(r, dst); err != nil {
		return err
	}

	if err == nil {
		t.log("db.tx.query.cache.add", t.tid, nil, time.Since(start), query)
		t.cache[key] = reflect.ValueOf(dst).Elem()
		return nil
	}

	t.log("db.tx.query", t.tid, err, time.Since(start), query)
	return err
}

// Commit the transaction.
func (t *Tx) Commit() (err error) {
	start := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	err = t.tx.Commit()
	t.done = true

	t.log("db.tx.commit", t.tid, err, time.Since(start), "")
	return err
}

// Rollback aborts the transaction.
func (t *Tx) Rollback() (err error) {
	start := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.done {
		return nil
	}

	err = t.tx.Rollback()
	t.done = true

	t.log("db.tx.rollback", t.tid, err, time.Since(start), "")
	return err
}
