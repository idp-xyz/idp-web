package state_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-web/internal/pgtest"
	"idp-web/internal/state"
)

// These tests run on a real PostgreSQL (make test-integration) and are skipped without one. A "replica" is a Store of its
// own: its own pool and connections, the same database, which is what two idp-web pods are.

var noon = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// wide gives a replica enough connections for its goroutines to really run at the same time in the database.
func wide(u string) string { return u + "&pool_max_conns=16" }

// usedOn reads what the table says, as the owner: not through the code under test.
func usedOn(t *testing.T, owner, tenant, kind, day string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var used int
	err = conn.QueryRow(context.Background(), `SELECT used FROM daily_usage WHERE tenant = $1 AND kind = $2 AND day = $3::date`, tenant, kind, day).Scan(&used)
	if err == pgx.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return used
}

// The decision "is there quota left, and take one" is one statement: of many calls at once, from two replicas, exactly the
// quota is allowed, never one more and never one fewer. Every round is a new tenant, every goroutine is released at the same
// moment, and each makes several attempts, so the attempts overlap in the database. A take that read the count and then
// wrote it would let more than the quota through in the first round.
func TestTwoReplicasConsumingOneTenantsQuotaAtTheSameTimeAllowExactlyTheQuota(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	replicas := []*state.Store{pgtest.Open(t, wide(url), state.Options{}), pgtest.Open(t, wide(url), state.Options{})}
	const rounds, quota, goroutines, attempts = 25, 50, 32, 4 // 2 x 32 x 4 = 256 attempts for 50 units
	for round := 0; round < rounds; round++ {
		tenant := fmt.Sprintf("tenant-%d", round)
		var allowed, refused, failed atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, r := range replicas {
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for i := 0; i < attempts; i++ {
						ok, err := r.Take(context.Background(), tenant, state.Searches, noon, quota)
						switch {
						case err != nil:
							failed.Add(1)
						case ok:
							allowed.Add(1)
						default:
							refused.Add(1)
						}
					}
				}()
			}
		}
		close(start)
		wg.Wait()
		if allowed.Load() != quota || refused.Load() != 2*goroutines*attempts-quota || failed.Load() != 0 {
			t.Fatalf("round %d: %d allowed, %d refused, %d failed; the quota is %d", round, allowed.Load(), refused.Load(), failed.Load(), quota)
		}
		if got := usedOn(t, owner, tenant, state.Searches, "2026-11-19"); got != quota {
			t.Fatalf("round %d: the table says %d used, the quota is %d", round, got, quota)
		}
	}
}

func TestSearchesAndReadsAndTenantsAreCountedApart(t *testing.T) {
	_, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{})
	take := func(tenant, kind string) bool {
		ok, err := s.Take(context.Background(), tenant, kind, noon, 1)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !take("a", state.Searches) || !take("a", state.Reads) || !take("b", state.Searches) {
		t.Fatal("a unit of another kind or of another tenant was counted against this one")
	}
	if take("a", state.Searches) || take("a", state.Reads) || take("b", state.Searches) {
		t.Fatal("a second unit was allowed under a limit of one")
	}
}

func TestTheCountSurvivesARestart(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	before := pgtest.Open(t, url, state.Options{})
	for i := 0; i < 3; i++ {
		if ok, err := before.Take(context.Background(), "acme", state.Reads, noon, 4); err != nil || !ok {
			t.Fatalf("take %d: %v %v", i, ok, err)
		}
	}
	before.Close() // the process ends
	after := pgtest.Open(t, url, state.Options{})
	if ok, _ := after.Take(context.Background(), "acme", state.Reads, noon, 4); !ok {
		t.Fatal("the fourth of four was refused after a restart")
	}
	if ok, _ := after.Take(context.Background(), "acme", state.Reads, noon, 4); ok {
		t.Fatal("a fifth was allowed: the count did not survive the restart")
	}
	if got := usedOn(t, owner, "acme", state.Reads, "2026-11-19"); got != 4 {
		t.Fatalf("used %d", got)
	}
}

// The day is the UTC calendar day: the count of the next day starts at zero, and a moment is put in its day by UTC whatever
// zone it is written in.
func TestTheNextUTCDayStartsAtZeroAndADayIsTheUTCDayWhateverTheZone(t *testing.T) {
	_, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{})
	take := func(at time.Time) bool {
		ok, err := s.Take(context.Background(), "acme", state.Searches, at, 2)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	lastSecond := time.Date(2026, 11, 19, 23, 59, 59, 0, time.UTC)
	if !take(lastSecond) || !take(lastSecond.Add(-time.Hour)) || take(lastSecond) {
		t.Fatal("two units of a day were not exactly two")
	}
	if !take(lastSecond.Add(time.Second)) || !take(lastSecond.Add(time.Hour)) || take(lastSecond.Add(2*time.Hour)) {
		t.Fatal("the next day did not start at zero with its own two")
	}
	// 05:00 on the 20th in UTC+8 is 21:00 on the 19th in UTC: the 19th's count, which is used up
	east := time.FixedZone("UTC+8", 8*3600)
	if take(time.Date(2026, 11, 20, 5, 0, 0, 0, east)) {
		t.Fatal("a moment in another zone was counted on the day of that zone, not on its UTC day")
	}
	// 08:30 on the 20th in UTC+8 is 00:30 on the 20th in UTC, used up as well
	if take(time.Date(2026, 11, 20, 8, 30, 0, 0, east)) {
		t.Fatal("the UTC day 2026-11-20 had been used up")
	}
	// ...and the day after has its own
	if !take(time.Date(2026, 11, 21, 0, 0, 1, 0, time.UTC)) {
		t.Fatal("the day after did not start at zero")
	}
}

// 0 means none allowed. The first call of the day inserts the row, so the insert has to refuse as well as the update.
func TestALimitOfZeroAllowsNothingEvenOnTheFirstCallOfTheDayAndALowerLimitRefusesWhatWasUsed(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{})
	if ok, err := s.Take(context.Background(), "none", state.Searches, noon, 0); err != nil || ok {
		t.Fatalf("a limit of 0 allowed a call: %v %v", ok, err)
	}
	if got := usedOn(t, owner, "none", state.Searches, "2026-11-19"); got != 0 {
		t.Fatalf("used %d under a limit of 0", got)
	}
	for i := 0; i < 5; i++ {
		s.Take(context.Background(), "lowered", state.Searches, noon, 10)
	}
	if ok, _ := s.Take(context.Background(), "lowered", state.Searches, noon, 3); ok {
		t.Fatal("a limit lowered below what was used allowed a call")
	}
	if got := usedOn(t, owner, "lowered", state.Searches, "2026-11-19"); got != 5 {
		t.Fatalf("a refused call changed the count to %d", got)
	}
}

func TestAUnitGivenBackIsAvailableAgainAndTheCountNeverGoesBelowZero(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{})
	ctx := context.Background()
	s.Take(ctx, "acme", state.Reads, noon, 1)
	if ok, _ := s.Take(ctx, "acme", state.Reads, noon, 1); ok {
		t.Fatal("over the limit")
	}
	if err := s.Release(ctx, "acme", state.Reads, noon); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Take(ctx, "acme", state.Reads, noon, 1); !ok {
		t.Fatal("a unit given back was not available")
	}
	for i := 0; i < 3; i++ {
		if err := s.Release(ctx, "acme", state.Reads, noon); err != nil {
			t.Fatal(err)
		}
	}
	if got := usedOn(t, owner, "acme", state.Reads, "2026-11-19"); got != 0 {
		t.Fatalf("used %d after more releases than takes", got)
	}
	if err := s.Release(ctx, "nobody", state.Reads, noon); err != nil {
		t.Fatalf("giving back where nothing was taken: %v", err)
	}
}

// ---- numbering ----

func number(t *testing.T, s *state.Store, caller string, at time.Time, addrs ...string) []int {
	t.Helper()
	n, err := s.Number(context.Background(), caller, addrs, at)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Two replicas that serve the same caller give an address the same number, and different addresses different ones, even
// when they ask at the same time and in different orders: the numbers are exactly 1 to N, each once.
func TestTwoReplicasGiveTheSameNumberToTheSameAddressAndDifferentNumbersToDifferentOnesUnderConcurrency(t *testing.T) {
	_, url := pgtest.Scratch(t)
	replicas := []*state.Store{pgtest.Open(t, wide(url), state.Options{}), pgtest.Open(t, wide(url), state.Options{})}
	const addresses, goroutines = 25, 24
	addr := func(i int) string { return fmt.Sprintf("https://example.com/page/%d", i) }
	for round, caller := range []string{"t1/alice", "t1/bob", "t2/alice"} {
		var mu sync.Mutex
		got := map[string]map[int]bool{} // address -> the numbers it was given
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, r := range replicas {
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					order := make([]string, addresses) // each goroutine asks for all of them in its own order, in batches of 1 to 5
					for i := range order {
						order[i] = addr((i*7 + g*3 + round) % addresses)
					}
					<-start
					for i := 0; i < len(order); i += 1 + g%5 {
						end := min(i+1+g%5, len(order))
						ns, err := r.Number(context.Background(), caller, order[i:end], noon)
						if err != nil {
							t.Error(err)
							return
						}
						mu.Lock()
						for k, n := range ns {
							if got[order[i+k]] == nil {
								got[order[i+k]] = map[int]bool{}
							}
							got[order[i+k]][n] = true
						}
						mu.Unlock()
					}
				}(g)
			}
		}
		close(start)
		wg.Wait()
		seen := map[int]string{}
		for a, ns := range got {
			if len(ns) != 1 {
				t.Fatalf("%s: %s was given the numbers %v", caller, a, ns)
			}
			for n := range ns {
				if other, dup := seen[n]; dup {
					t.Fatalf("%s: the number %d names %s and %s", caller, n, other, a)
				}
				seen[n] = a
			}
		}
		if len(got) != addresses {
			t.Fatalf("%s: %d addresses numbered of %d", caller, len(got), addresses)
		}
		for n := 1; n <= addresses; n++ {
			if seen[n] == "" {
				t.Fatalf("%s: the number %d was skipped", caller, n)
			}
		}
	}
}

func TestTheNumbersSurviveARestartAndAnotherReplicaSeesThem(t *testing.T) {
	_, url := pgtest.Scratch(t)
	a := pgtest.Open(t, url, state.Options{})
	first := number(t, a, "t/alice", noon, "https://a.example/", "https://b.example/", "https://c.example/")
	a.Close()
	b := pgtest.Open(t, url, state.Options{}) // another process, later
	again := number(t, b, "t/alice", noon.Add(time.Minute), "https://c.example/", "https://d.example/", "https://a.example/")
	if fmt.Sprint(first) != "[1 2 3]" || fmt.Sprint(again) != "[3 4 1]" {
		t.Fatalf("first %v, after the restart %v", first, again)
	}
	if other := number(t, b, "t/bob", noon, "https://c.example/"); other[0] != 1 {
		t.Fatalf("another caller's numbering started at %d, not 1", other[0])
	}
}

// The lifetime of a numbering is what D19 says: it is forgotten after an hour without use and starts again at 1; every use
// renews the hour; at most MaxAddresses addresses are kept, the one used longest ago is dropped first, and the number of a
// dropped address is not given again.
func TestANumberingIsForgottenAfterAnHourWithoutUseRenewedByUseAndBoundedInAddresses(t *testing.T) {
	_, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{MaxAddresses: 3})
	n := number(t, s, "t/a", noon, "https://1.example/", "https://2.example/")
	number(t, s, "t/a", noon.Add(50*time.Minute), "https://1.example/") // used again: the hour starts over
	if got := number(t, s, "t/a", noon.Add(100*time.Minute), "https://2.example/")[0]; got != n[1] {
		t.Fatalf("a numbering used 50 minutes before was forgotten: %d, was %d", got, n[1])
	}
	if got := number(t, s, "t/a", noon.Add(100*time.Minute+61*time.Minute), "https://2.example/")[0]; got != 1 {
		t.Fatalf("after more than an hour without use the numbering did not start again: %d", got)
	}
	// the bound: 3 addresses are kept; the fourth drops the one used longest ago, and its number is not reused
	at := noon.Add(10 * time.Hour)
	number(t, s, "t/b", at, "https://a.example/")
	number(t, s, "t/b", at.Add(time.Second), "https://b.example/")
	number(t, s, "t/b", at.Add(2*time.Second), "https://c.example/")
	number(t, s, "t/b", at.Add(3*time.Second), "https://a.example/") // a is recent, b is the oldest
	if got := number(t, s, "t/b", at.Add(4*time.Second), "https://d.example/")[0]; got != 4 {
		t.Fatalf("the fourth address got %d", got)
	}
	if got := number(t, s, "t/b", at.Add(5*time.Second), "https://a.example/", "https://c.example/")[0]; got != 1 {
		t.Fatalf("the address that was used recently was dropped: %d", got)
	}
	if got := number(t, s, "t/b", at.Add(6*time.Second), "https://b.example/")[0]; got != 5 {
		t.Fatalf("the dropped address came back as %d, not the next free number 5: a number was reused", got)
	}
}

// The database keeps the caller and the address as digests and nothing else: not the address, not the subject, not the
// query. This reads every table as the owner, as text, the way an operator with access to the database would.
func TestWhatIsStoredIsTheHashesAndNotTheAddressTheCallerOrTheQuery(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{})
	ctx := context.Background()
	const caller, address, query = "t-secret-tenant/alice-the-subject", "https://very-private.example.com/secret/page?token=abc", "the member's confidential question"
	number(t, s, caller, noon, address)
	if ok, err := s.Take(ctx, "t-secret-tenant", state.Searches, noon, 5); err != nil || !ok {
		t.Fatal(ok, err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var dump strings.Builder
	for _, table := range []string{"daily_usage", "source_callers", "source_numbers"} {
		rows, err := conn.Query(ctx, `SELECT t::text FROM `+table+` t`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			rows.Scan(&line)
			dump.WriteString(table + " " + line + "\n")
		}
		rows.Close()
	}
	// a bytea column is printed in hex, so the text of an address stored raw would not show as itself
	for _, secret := range []string{address, "very-private", "alice", "token=abc", query, "confidential",
		hex.EncodeToString([]byte(address)), hex.EncodeToString([]byte(caller)), hex.EncodeToString([]byte("very-private"))} {
		if strings.Contains(dump.String(), secret) {
			t.Errorf("the database holds %q:\n%s", secret, dump.String())
		}
	}
	var gotCaller, gotAddress []byte
	if err := conn.QueryRow(ctx, `SELECT c.caller, n.address FROM source_callers c JOIN source_numbers n USING (caller)`).Scan(&gotCaller, &gotAddress); err != nil {
		t.Fatal(err)
	}
	wantCaller, wantAddress := sha256.Sum256([]byte(caller)), sha256.Sum256([]byte(address))
	if string(gotCaller) != string(wantCaller[:]) || string(gotAddress) != string(wantAddress[:]) {
		t.Errorf("the rows hold %x and %x, not the SHA-256 of the caller and of the address", gotCaller, gotAddress)
	}
	if !strings.Contains(dump.String(), "t-secret-tenant") {
		t.Errorf("the quota row names its tenant (the key of the count):\n%s", dump.String())
	}
}

// What a cleanup removes, and what it leaves: numberings idle for more than their hour (with their addresses), quota days
// older than the keep; a live numbering and recent days stay.
func TestTheSweepRemovesIdleNumberingsAndOldQuotaDaysAndNothingElse(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{})
	ctx := context.Background()
	number(t, s, "t/old", noon.Add(-2*time.Hour), "https://x.example/", "https://y.example/")
	number(t, s, "t/live", noon.Add(-10*time.Minute), "https://x.example/")
	for _, d := range []int{-60, -36, -34, -1, 0} {
		if ok, err := s.Take(ctx, "acme", state.Searches, noon.AddDate(0, 0, d), 5); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	callers, usage, err := s.Sweep(ctx, noon)
	if err != nil || callers != 1 || usage != 2 {
		t.Fatalf("swept %d numberings and %d quota days (%v): want 1 and 2", callers, usage, err)
	}
	conn, _ := pgx.Connect(ctx, owner)
	defer conn.Close(ctx)
	var nc, na, nu int
	conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM source_callers), (SELECT count(*) FROM source_numbers), (SELECT count(*) FROM daily_usage)`).Scan(&nc, &na, &nu)
	if nc != 1 || na != 1 || nu != 3 {
		t.Fatalf("left: %d numberings, %d addresses, %d quota days", nc, na, nu)
	}
	// a numbering swept while its caller is back: the next call starts a numbering again at 1 and does not fail
	if got := number(t, s, "t/old", noon, "https://y.example/"); got[0] != 1 {
		t.Fatalf("%v", got)
	}
}

// A database that is slow is an error within the timeout, not a call that waits: here a lock held on the quota table by
// someone else stops every statement, and the store gives up after its 300 ms. A store whose database is gone answers at once.
func TestASlowDatabaseIsAnErrorWithinTheTimeoutAndAClosedOneAnswersAtOnce(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	s := pgtest.Open(t, url, state.Options{Timeout: 300 * time.Millisecond})
	ctx := context.Background()
	blocker, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE daily_usage, source_callers IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if ok, err := s.Take(ctx, "acme", state.Searches, noon, 5); err == nil || ok {
		t.Fatalf("a take under a lock on the table: %v %v", ok, err)
	}
	if _, err := s.Number(ctx, "t/a", []string{"x"}, noon); err == nil {
		t.Fatal("numbering under a lock on the table")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("two calls under a lock took %v with a timeout of 300 ms", d)
	}
	_ = tx.Rollback(ctx)
	if ok, err := s.Take(ctx, "acme", state.Searches, noon, 5); err != nil || !ok {
		t.Fatalf("the store did not recover when the lock went: %v %v", ok, err)
	}
	s.Close()
	start = time.Now()
	if _, err := s.Take(ctx, "acme", state.Searches, noon, 5); err == nil {
		t.Fatal("a closed store allowed a call")
	}
	if _, err := s.Number(ctx, "t/a", []string{"x"}, noon); err == nil {
		t.Fatal("a closed store numbered")
	}
	if err := s.Ping(ctx); err == nil {
		t.Fatal("a closed store answered a ping")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a closed store took %v", time.Since(start))
	}
}

func TestOpenRefusesASuperuserAnUnmigratedSchemaAndAnUnreachableDatabase(t *testing.T) {
	owner, url := pgtest.Scratch(t)
	if s, err := state.Open(context.Background(), owner, state.Options{}); err == nil {
		s.Close()
		t.Error("opened as a superuser")
	}
	if s, err := state.Open(context.Background(), url, state.Options{}); err != nil {
		t.Errorf("the service role was refused: %v", err)
	} else {
		s.Close()
	}
	conn, _ := pgx.Connect(context.Background(), owner)
	defer conn.Close(context.Background())
	conn.Exec(context.Background(), `DELETE FROM schema_migrations`)
	if s, err := state.Open(context.Background(), url, state.Options{}); err == nil || !strings.Contains(err.Error(), "idp-web migrate") {
		if s != nil {
			s.Close()
		}
		t.Errorf("an unmigrated schema: %v", err)
	}
	if s, err := state.Open(context.Background(), "postgres://idp_web_it_svc:x@127.0.0.1:1/none?connect_timeout=1", state.Options{}); err == nil {
		s.Close()
		t.Error("opened a database that is not there")
	}
}

func TestTheMigrationsApplyOnceAndTheServiceRoleHasOnlyTheTablePrivilegesItNeeds(t *testing.T) {
	owner, url := pgtest.Scratch(t) // applied once by Scratch
	applied, err := state.Migrate(context.Background(), owner)
	if err != nil || len(applied) != 0 {
		t.Fatalf("a second run applied %v (%v)", applied, err)
	}
	s := pgtest.Open(t, url, state.Options{})
	_ = s
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, sql := range []string{`DROP TABLE daily_usage`, `CREATE TABLE intruder (x int)`, `TRUNCATE daily_usage`, `ALTER TABLE daily_usage ADD COLUMN query text`} {
		if _, err := conn.Exec(context.Background(), sql); err == nil {
			t.Errorf("the service role could run %q", sql)
		}
	}
}
