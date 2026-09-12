package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB is an in-memory stand-in for the two tables the api-gateway OWNS
// (idempotency_keys and leases), sufficient to test the decision logic in the
// query layer without a live Postgres.
//
// It exists because the behaviours under test — same-key/different-body 409, and
// stale-lease-token rejection — are the two places where getting the logic subtly
// wrong silently corrupts state. Those deserve fast, deterministic, always-runnable
// tests, not tests that are skipped on any machine without a database. The
// integration tests in gateway_integration_test.go then verify the SQL itself
// against real Postgres.
//
// The fake recognizes statements by a distinctive substring rather than by parsing
// SQL: brittle if the queries are rewritten, which is exactly the signal we want —
// a rewritten query should force a look at whether its contract still holds.
type fakeDB struct {
	mu sync.Mutex

	idem   map[string]*fakeIdem
	leases map[string]*fakeLease
	now    time.Time
}

type fakeIdem struct {
	requestHash string
	response    string
}

type fakeLease struct {
	id        string
	attemptID string
	workerID  string
	token     string
	expiresAt time.Time
	epoch     int32
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		idem:   make(map[string]*fakeIdem),
		leases: make(map[string]*fakeLease),
		now:    time.Now(),
	}
}

func (f *fakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.Contains(sql, "INSERT INTO idempotency_keys"):
		key := idemKey(args[0].(string), args[1].(string))
		if _, exists := f.idem[key]; exists {
			// ON CONFLICT DO NOTHING: zero rows affected is how the caller learns
			// it lost the claim.
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
		f.idem[key] = &fakeIdem{requestHash: args[2].(string), response: "null"}
		return pgconn.NewCommandTag("INSERT 0 1"), nil

	case strings.Contains(sql, "UPDATE idempotency_keys"):
		key := idemKey(args[0].(string), args[1].(string))
		row, exists := f.idem[key]
		if !exists {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		row.response = args[2].(string)
		return pgconn.NewCommandTag("UPDATE 1"), nil

	case strings.Contains(sql, "DELETE FROM idempotency_keys"):
		key := idemKey(args[0].(string), args[1].(string))
		row, exists := f.idem[key]
		// The real statement only deletes a claim with no cached response, so a
		// completed key is never released.
		if !exists || row.response != "null" {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.idem, key)
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.NewCommandTag(""), fmt.Errorf("fakeDB: unhandled Exec: %s", sql)
}

func (f *fakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.Contains(sql, "FROM idempotency_keys"):
		key := idemKey(args[0].(string), args[1].(string))
		row, exists := f.idem[key]
		if !exists {
			return errRow{pgx.ErrNoRows}
		}
		return valueRow{[]any{row.requestHash, []byte(row.response)}}

	case strings.Contains(sql, "FROM leases"):
		lease, exists := f.leases[args[0].(string)]
		if !exists {
			return errRow{pgx.ErrNoRows}
		}
		expired := !lease.expiresAt.After(f.now)
		return valueRow{[]any{
			lease.id, lease.attemptID, lease.workerID, lease.token,
			lease.expiresAt, lease.epoch, expired,
		}}

	case strings.Contains(sql, "UPDATE leases"):
		attemptID := args[0].(string)
		workerID := args[1].(string)
		ttlSecs := args[2].(float64)
		token := args[3].(string)
		lease, exists := f.leases[attemptID]
		// The real UPDATE is guarded by attempt AND worker AND token AND
		// not-yet-expired; anything else matches no row and becomes a revocation.
		if !exists || lease.workerID != workerID || lease.token != token || !lease.expiresAt.After(f.now) {
			return errRow{pgx.ErrNoRows}
		}
		lease.expiresAt = f.now.Add(time.Duration(ttlSecs) * time.Second)
		return valueRow{[]any{lease.expiresAt, lease.epoch}}
	}
	return errRow{fmt.Errorf("fakeDB: unhandled QueryRow: %s", sql)}
}

func (f *fakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("fakeDB: unhandled Query: %s", sql)
}

func (f *fakeDB) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, fmt.Errorf("fakeDB: transactions are not modelled")
}

func idemKey(ns, key string) string { return ns + "\x00" + key }

// putLease seeds a live lease.
func (f *fakeDB) putLease(attemptID, workerID, token string, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases[attemptID] = &fakeLease{
		id:        "lease-" + attemptID,
		attemptID: attemptID,
		workerID:  workerID,
		token:     token,
		expiresAt: f.now.Add(ttl),
		epoch:     1,
	}
}

// advance moves the fake clock, which is how lease expiry is tested without
// sleeping.
func (f *fakeDB) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// rotateLease simulates the control plane reclaiming an expired lease and
// re-issuing it: a NEW token for the SAME attempt, which is exactly the situation
// fencing exists for.
func (f *fakeDB) rotateLease(attemptID, newWorkerID, newToken string, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.leases[attemptID]
	epoch := int32(1)
	if old != nil {
		epoch = old.epoch + 1
	}
	f.leases[attemptID] = &fakeLease{
		id:        "lease-" + attemptID,
		attemptID: attemptID,
		workerID:  newWorkerID,
		token:     newToken,
		expiresAt: f.now.Add(ttl),
		epoch:     epoch,
	}
}

// ── minimal pgx.Row implementations ───────────────────────────────────────

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

type valueRow struct{ vals []any }

func (r valueRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("fakeDB: scan wants %d destinations, row has %d", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch t := d.(type) {
		case *string:
			s, ok := r.vals[i].(string)
			if !ok {
				return fmt.Errorf("fakeDB: column %d is %T, want string", i, r.vals[i])
			}
			*t = s
		case *[]byte:
			b, ok := r.vals[i].([]byte)
			if !ok {
				return fmt.Errorf("fakeDB: column %d is %T, want []byte", i, r.vals[i])
			}
			*t = b
		case *time.Time:
			v, ok := r.vals[i].(time.Time)
			if !ok {
				return fmt.Errorf("fakeDB: column %d is %T, want time.Time", i, r.vals[i])
			}
			*t = v
		case *int32:
			v, ok := r.vals[i].(int32)
			if !ok {
				return fmt.Errorf("fakeDB: column %d is %T, want int32", i, r.vals[i])
			}
			*t = v
		case *bool:
			v, ok := r.vals[i].(bool)
			if !ok {
				return fmt.Errorf("fakeDB: column %d is %T, want bool", i, r.vals[i])
			}
			*t = v
		default:
			return fmt.Errorf("fakeDB: unsupported scan destination %T", d)
		}
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
