package pgx

import (
	"math"
	"strings"
	"time"

	"github.com/EndFirstCorp/onedb"
	"github.com/pkg/errors"
	pgx "gopkg.in/jackc/pgx.v2"
)

type pgxWrapper interface {
	Begin() (Txer, error)
	Close()
	querier
}

type querier interface {
	Exec(query string, args ...interface{}) (CommandTag, error)
	Query(query string, args ...interface{}) (onedb.RowsScanner, error)
	QueryRow(query string, args ...interface{}) onedb.Scanner
	CopyFrom(tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int, error)
}

// Rower is the public interface for all the capability found in a *pgx.Rows. Note that the Close method
// returns an error similar to how database/sql's rows Close returns and error
type Rower interface {
	AfterClose(f func(Rower)) // modified from pgxRower to use interface
	Close() error             // modified from pgxRower to match database/sql
	Conn() *pgx.Conn
	Err() error
	Fatal(err error)
	FieldDescriptions() []FieldDescription
	Next() bool
	onedb.Scanner
	Values() ([]interface{}, error)

	Columns() ([]string, error) // added
}

type pgxRower interface {
	AfterClose(f func(*pgx.Rows))
	Close()
	Conn() *pgx.Conn
	Err() error
	Fatal(err error)
	FieldDescriptions() []pgx.FieldDescription
	Next() bool
	onedb.Scanner
	Values() ([]interface{}, error)
}

type pgxWithReconnect struct {
	db         *pgx.ConnPool
	lastRetry  time.Time
	retryCount int
	pgxWrapper
}

// PGX Errors, reexported for convenience. Documentation is copied as written.

// ErrNoRows occurs when rows are expected but none are returned.
var ErrNoRows = pgx.ErrNoRows

// ErrNotificationTimeout occurs when WaitForNotification times out.
var ErrNotificationTimeout = pgx.ErrNotificationTimeout

// ErrDeadConn occurs on an attempt to use a dead connection
var ErrDeadConn = pgx.ErrDeadConn

// ErrTLSRefused occurs when the connection attempt requires TLS and the
// PostgreSQL server refuses to use TLS
var ErrTLSRefused = pgx.ErrTLSRefused

// ErrConnBusy occurs when the connection is busy (for example, in the middle of
// reading query results) and another action is attempts.
var ErrConnBusy = pgx.ErrConnBusy

// ErrInvalidLogLevel occurs on attempt to set an invalid log level.
var ErrInvalidLogLevel = pgx.ErrInvalidLogLevel

// ProtocolError occurs when unexpected data is received from PostgreSQL
type ProtocolError pgx.ProtocolError

func (b *pgxWithReconnect) Begin() (Txer, error) {
	t, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: t}, err
}

func (b *pgxWithReconnect) Close() {
	b.db.Close()
}

func (b *pgxWithReconnect) CopyFrom(tableName Identifier, columnNames []string, rows CopyFromSource) (int, error) {
	return b.db.CopyFrom(pgx.Identifier(onedb.LowerSlice(tableName)), onedb.LowerSlice(columnNames), rows)
}

func (b *pgxWithReconnect) QueryRow(query string, args ...interface{}) onedb.Scanner {
	return b.db.QueryRow(query, args...)
}

func (b *pgxWithReconnect) Query(query string, args ...interface{}) (onedb.RowsScanner, error) {
	rows, err := b.db.Query(query, args...)
	if (err == pgx.ErrDeadConn || err != nil && strings.HasSuffix(err.Error(), "connection reset by peer")) && b.reconnect() {
		return b.Query(query)
	} else if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, rows.Err()
}

func (b *pgxWithReconnect) Exec(query string, args ...interface{}) (CommandTag, error) {
	tag, err := b.db.Exec(query, args...)
	if (err == pgx.ErrDeadConn || err != nil && strings.HasSuffix(err.Error(), "connection reset by peer")) && b.reconnect() {
		return b.Exec(query, args...)
	}
	return CommandTag(tag), err
}

func (b *pgxWithReconnect) ping() error {
	var val int
	if err := b.db.QueryRow("select 1 + 1").Scan(&val); err != nil {
		return err
	}
	if val != 2 {
		return errors.New("Failed ping test")
	}
	return nil
}

func (b *pgxWithReconnect) reconnect() bool {
	ms := time.Millisecond * time.Duration(math.Pow10(b.retryCount)) // retry every 10^lastRetry milliseconds
	if time.Since(b.lastRetry) > ms {
		b.lastRetry = time.Now()
		err := b.ping()
		if err == nil {
			b.retryCount = 0
			return true
		} else if b.retryCount < 4 { // max retry time is 10 seconds
			b.retryCount++
		}
	}
	return false
}

type pgxRows struct {
	rows pgxRower
	Rower
}

// AfterClose adds f to a LILO queue of functions that will be called when
// rows is closed.
func (r *pgxRows) AfterClose(f func(Rower)) {}

func (r *pgxRows) Columns() ([]string, error) {
	fields := r.rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, field := range fields {
		columns[i] = field.Name
	}
	return columns, nil
}

// Next prepares the next row for reading. It returns true if there is another
// row and false if no more rows are available. It automatically closes rows
// when all rows are read.
func (r *pgxRows) Next() bool {
	return r.rows.Next()
}

// Close closes the rows, making the connection ready for use again. It is safe
// to call Close after rows is already closed.
func (r *pgxRows) Close() error {
	r.rows.Close()
	return nil
}

// Conn returns the *Conn this *Rows is using.
func (r *pgxRows) Conn() *pgx.Conn {
	return r.rows.Conn()
}

// Fatal signals an error occurred after the query was sent to the server. It
// closes the rows automatically.
func (r *pgxRows) Fatal(err error) {
	r.rows.Fatal(err)
}

func (r *pgxRows) FieldDescriptions() []FieldDescription {
	descriptions := r.rows.FieldDescriptions()
	result := make([]FieldDescription, len(descriptions))
	for i := 0; i < len(descriptions); i++ {
		d := descriptions[i]
		result[i] = FieldDescription{
			Name:            d.Name,
			Table:           Oid(d.Table),
			AttributeNumber: d.AttributeNumber,
			DataType:        Oid(d.DataType),
			DataTypeSize:    d.DataTypeSize,
			DataTypeName:    d.DataTypeName,
			Modifier:        d.Modifier,
			FormatCode:      d.FormatCode,
		}
	}
	return result
}

// Scan works the same as (*Rows Scan) with the following exceptions. If no
// rows were found it returns ErrNoRows. If multiple rows are returned it
// ignores all but the first.
func (r *pgxRows) Scan(dest ...interface{}) error {
	vals, err := r.rows.Values()
	if err != nil {
		return err
	}
	for i, item := range dest {
		*(item.(*interface{})) = vals[i]
	}
	return nil
}

func (r *pgxRows) Values() ([]interface{}, error) {
	return r.rows.Values()
}

func (r *pgxRows) Err() error {
	return r.rows.Err()
}
