// Package state is idp-web's one piece of shared, durable state: the daily quota of searches and page reads per tenant,
// and the numbers [n] of the sources a caller has been shown (ADR-0001 D28, D29, platform ADR-0003 appendix D 4.4).
// It is PostgreSQL, so that the count survives a restart and is the same for every replica.
//
// Nothing here is read-then-write. Taking a unit of quota is one statement, and giving a number is one transaction that
// first takes the caller's row lock, so that two replicas asking for the same caller are served one after the other.
//
// What is stored is counts, numbers and SHA-256 digests of the caller and of the address. The query, the address, the
// page and the caller's subject are never written to the database.
package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"idp-web/migrations"
)

// The kinds of call a quota counts.
const (
	Searches = "searches"
	Reads    = "reads"
)

// Defaults of Options.
const (
	DefaultTimeout      = 2 * time.Second // one statement or transaction: a slow database refuses the call, it does not hold it
	DefaultSourceTTL    = time.Hour       // a numbering is forgotten after this long without use (D19)
	DefaultMaxAddresses = 1000            // addresses kept per caller (D19)
	usageKeepDays       = 35              // rows of daily_usage older than this are swept; a month and some, for an operator's reconciliation
)

// Options are the knobs of a Store; a zero field takes its default.
type Options struct {
	Timeout      time.Duration
	SourceTTL    time.Duration
	MaxAddresses int
}

// Store is the database. It is safe for concurrent use; every replica has its own.
type Store struct {
	pool *pgxpool.Pool
	opt  Options
}

// Hash is how the caller and the address are stored: the SHA-256 of the text. The audit line (internal/tools, short)
// prints the first four bytes of the same digest of the caller, so a row can be matched to an audit line by that prefix.
func Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// Open connects as the service role. It refuses a superuser or a BYPASSRLS role (a service that can do anything is not
// the least-privilege role it is meant to run as), and a schema that is not at the version this binary needs.
func Open(ctx context.Context, url string, opt Options) (*Store, error) {
	if opt.Timeout == 0 {
		opt.Timeout = DefaultTimeout
	}
	if opt.SourceTTL == 0 {
		opt.SourceTTL = DefaultSourceTTL
	}
	if opt.MaxAddresses == 0 {
		opt.MaxAddresses = DefaultMaxAddresses
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	guardDial(&cfg.ConnConfig.Config) // a peer that vanished is noticed in seconds, not when TCP gives up
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, opt: opt}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var super, bypass bool
	if err := pool.QueryRow(c, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable or role unknown: %w", err)
	}
	if super || bypass {
		pool.Close()
		return nil, errors.New("refusing to run as a superuser or BYPASSRLS role: connect as a member of idp_web_app")
	}
	var version int
	if err := pool.QueryRow(c, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema not migrated (run idp-web migrate): %w", err)
	}
	if want := latest(); version != want {
		pool.Close()
		return nil, fmt.Errorf("schema at version %d, binary needs %d (run idp-web migrate)", version, want)
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping asks the database a question, within the store's timeout: what /readyz reports.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.opt.Timeout)
	defer cancel()
	return s.pool.Ping(ctx)
}

// day is the UTC calendar day of t, which is what a quota is counted over.
func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// takeSQL is the whole decision "is there quota left, and take one": one statement, never a read followed by a write.
// A first call of the day inserts the row with one unit used, unless the limit is not above zero. Every later call goes
// through ON CONFLICT ... DO UPDATE ... WHERE, which PostgreSQL runs under the row's lock and re-checks against the
// latest version of the row, so of any number of concurrent calls exactly the ones that find used < limit succeed. No
// row returned means the quota is used up.
const takeSQL = `
INSERT INTO daily_usage (tenant, day, kind, used)
SELECT $1::text, $2::date, $3::text, 1 WHERE $4::int > 0
ON CONFLICT (tenant, day, kind) DO UPDATE SET used = daily_usage.used + 1
  WHERE daily_usage.used < $4::int
RETURNING used`

// Take takes one unit of the tenant's quota of kind for the UTC day of now, if used < limit, and says whether it did.
// A limit of zero or less allows nothing. An error means the database did not answer in time or at all: the caller
// must then refuse, because the call is not counted.
func (s *Store) Take(ctx context.Context, tenant, kind string, now time.Time, limit int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opt.Timeout)
	defer cancel()
	var used int
	err := s.pool.QueryRow(ctx, takeSQL, tenant, day(now), kind, limit).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Release gives back a unit that Take took for a call that, it turned out, sent nothing to the web (ADR-0001 D28).
// It never goes below zero.
func (s *Store) Release(ctx context.Context, tenant, kind string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, s.opt.Timeout)
	defer cancel()
	_, err := s.pool.Exec(ctx, `UPDATE daily_usage SET used = used - 1 WHERE tenant = $1 AND day = $2::date AND kind = $3 AND used > 0`,
		tenant, day(now), kind)
	return err
}

// Number gives the caller's number for each address, in the order asked, and a new address the next free number of that
// caller. The same (caller, address) gets the same number from every replica and after every restart, for as long as the
// numbering lives: it is forgotten after SourceTTL without use and then starts again at 1, and it keeps at most
// MaxAddresses addresses, the longest unused dropped first, their numbers never given again (the semantics of D19 as they
// were in memory).
//
// One transaction: the caller's row is locked first (FOR UPDATE), so two replicas asking for the same caller are served
// one after the other and cannot give one number to two addresses; callers do not wait for each other.
func (s *Store) Number(ctx context.Context, caller string, addresses []string, now time.Time) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opt.Timeout)
	defer cancel()
	ch := Hash(caller)
	var out []int
	for attempt := 0; ; attempt++ {
		var again bool
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			out, again, err = s.number(ctx, tx, ch, addresses, now)
			return err
		})
		if err == nil && !again {
			return out, nil
		}
		if err != nil || attempt >= 3 {
			if err == nil {
				err = errors.New("the caller's row kept disappearing")
			}
			return nil, err
		}
	}
}

// number is one attempt of Number; again is true when the sweep deleted the caller's row between the insert and the
// lock, which asks for another attempt.
func (s *Store) number(ctx context.Context, tx pgx.Tx, ch []byte, addresses []string, now time.Time) (nums []int, again bool, err error) {
	if _, err = tx.Exec(ctx, `INSERT INTO source_callers (caller, next, last_used) VALUES ($1, 1, $2) ON CONFLICT (caller) DO NOTHING`, ch, now); err != nil {
		return nil, false, err
	}
	var next int
	var last time.Time
	err = tx.QueryRow(ctx, `SELECT next, last_used FROM source_callers WHERE caller = $1 FOR UPDATE`, ch).Scan(&next, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if now.Sub(last) > s.opt.SourceTTL { // an hour without use: forgotten, the numbering starts again
		if _, err = tx.Exec(ctx, `DELETE FROM source_numbers WHERE caller = $1`, ch); err != nil {
			return nil, false, err
		}
		next = 1
	}
	added := false
	for _, a := range addresses {
		ah := Hash(a)
		var n int
		err = tx.QueryRow(ctx, `UPDATE source_numbers SET last_used = $3 WHERE caller = $1 AND address = $2 RETURNING n`, ch, ah, now).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			n = next
			next++
			added = true
			_, err = tx.Exec(ctx, `INSERT INTO source_numbers (caller, address, n, last_used) VALUES ($1, $2, $3, $4)`, ch, ah, n, now)
		}
		if err != nil {
			return nil, false, err
		}
		nums = append(nums, n)
	}
	if _, err = tx.Exec(ctx, `UPDATE source_callers SET next = $2, last_used = GREATEST(last_used, $3) WHERE caller = $1`, ch, next, now); err != nil {
		return nil, false, err
	}
	if added { // forget the addresses used longest ago beyond the bound; their numbers are not given again
		_, err = tx.Exec(ctx, `DELETE FROM source_numbers WHERE caller = $1 AND address IN
			(SELECT address FROM source_numbers WHERE caller = $1 ORDER BY last_used DESC, n DESC OFFSET $2)`, ch, s.opt.MaxAddresses)
		if err != nil {
			return nil, false, err
		}
	}
	return nums, false, nil
}

// Sweep deletes what nothing needs any more: the numberings idle for longer than their lifetime (their addresses go
// with them) and the quota rows of days long past. Without it the tables would grow for as long as the service runs.
func (s *Store) Sweep(ctx context.Context, now time.Time) (callers, usage int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `DELETE FROM source_callers WHERE last_used < $1`, now.Add(-s.opt.SourceTTL))
	if err != nil {
		return 0, 0, err
	}
	callers = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM daily_usage WHERE day < $1::date`, day(now).AddDate(0, 0, -usageKeepDays))
	if err != nil {
		return callers, 0, err
	}
	return callers, tag.RowsAffected(), nil
}

// Run sweeps every interval until ctx ends; each replica does, and the deletes are idempotent. A failed sweep is passed to
// onError and tried again at the next tick.
func (s *Store) Run(ctx context.Context, interval time.Duration, now func() time.Time, onError func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, err := s.Sweep(ctx, now()); err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
		}
	}
}

// ---- migrations -----------------------------------------------------------------------------------------

type migration struct {
	version int
	name    string
	sql     string
}

func load() []migration {
	entries, _ := fs.ReadDir(migrations.FS, ".")
	var out []migration
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, _ := migrations.FS.ReadFile(e.Name())
		out = append(out, migration{v, e.Name(), string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

func latest() int {
	m := load()
	if len(m) == 0 {
		return 0
	}
	return m[len(m)-1].version
}

// Migrate applies pending migrations as the schema owner, one transaction each, serialized by an advisory lock so that
// concurrent deployments cannot interleave (the same procedure as idp-mcp and idp-host).
func Migrate(ctx context.Context, ownerURL string) (applied []string, err error) {
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(72580)`); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(72580)`)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'idp_web_app') THEN GRANT SELECT ON schema_migrations TO idp_web_app; END IF; END $$`); err != nil {
		return nil, err
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return nil, err
	}
	for _, m := range load() {
		if m.version <= current {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if _, err := tx.Exec(ctx, `GRANT SELECT ON schema_migrations TO idp_web_app`); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied = append(applied, m.name)
	}
	return applied, nil
}

// EnsureLogin creates or updates a login role that is a member of idp_web_app (owner connection).
func EnsureLogin(ctx context.Context, ownerURL, user, password string) error {
	if !validIdent(user) {
		return errors.New("invalid role name")
	}
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, user).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	// role names cannot be bind parameters; validIdent restricts them to [a-z0-9_]
	if _, err := conn.Exec(ctx, fmt.Sprintf(`%s ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD %s`, verb, user, quoteLiteral(password))); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, fmt.Sprintf(`GRANT idp_web_app TO %s`, user))
	return err
}

func validIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
