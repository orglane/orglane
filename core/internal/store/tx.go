package store

import "github.com/jackc/pgx/v5"

// Tx returns the transaction q is bound to, if any. Domain packages use it
// to queue jobs in the transaction of a change (River InsertTx), the
// transactional outbox.
func (q *Queries) Tx() (pgx.Tx, bool) {
	tx, ok := q.db.(pgx.Tx)
	return tx, ok
}
