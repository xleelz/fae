package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, attempts int) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, m, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := m.ExpectationsWereMet(); e != nil {
			t.Error(e)
		}
		db.Close()
	})
	return New(db, Options{Attempts: attempts, Sleep: func(context.Context, time.Duration) error { return nil }}), m
}
func audit() Audit { return Audit{Actor: "operator", Action: "test", Outcome: "ok"} }
func expectAudit(m sqlmock.Sqlmock) {
	m.ExpectExec("INSERT INTO audit").WillReturnResult(sqlmock.NewResult(1, 1))
}
func TestMutationRetriesWholeBody(t *testing.T) {
	for failures := 0; failures < 5; failures++ {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			s, m := fixture(t, 5)
			for i := 0; i < failures; i++ {
				m.ExpectBegin()
				m.ExpectExec("UPDATE test").WillReturnError(&mysql.MySQLError{Number: 1213})
				m.ExpectRollback()
			}
			m.ExpectBegin()
			m.ExpectExec("UPDATE test").WillReturnResult(sqlmock.NewResult(0, 1))
			expectAudit(m)
			m.ExpectCommit()
			calls := 0
			e := s.Mutate(context.Background(), audit(), func(ctx context.Context, tx *sql.Tx) error {
				calls++
				_, e := tx.ExecContext(ctx, "UPDATE test")
				return e
			})
			if e != nil || calls != failures+1 {
				t.Fatalf("calls=%d err=%v", calls, e)
			}
		})
	}
}
func TestMutationCommitSerializationRetriesBody(t *testing.T) {
	s, m := fixture(t, 2)
	for i := 0; i < 2; i++ {
		m.ExpectBegin()
		expectAudit(m)
		if i == 0 {
			m.ExpectCommit().WillReturnError(&mysql.MySQLError{Number: 1213})
		} else {
			m.ExpectCommit()
		}
	}
	calls := 0
	if e := s.Mutate(context.Background(), audit(), func(context.Context, *sql.Tx) error { calls++; return nil }); e != nil || calls != 2 {
		t.Fatalf("%d %v", calls, e)
	}
}
func TestMutationBoundAndNonRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			s, m := fixture(t, 3)
			sentinel := errors.New("broken")
			var failure error = sentinel
			attempts := 1
			if retry {
				failure = &mysql.MySQLError{SQLState: [5]byte{'4', '0', '0', '0', '1'}}
				attempts = 3
			}
			for i := 0; i < attempts; i++ {
				m.ExpectBegin()
				m.ExpectRollback()
			}
			e := s.Mutate(context.Background(), audit(), func(context.Context, *sql.Tx) error { return failure })
			var transient *RetryableError
			if retry && !errors.As(e, &transient) {
				t.Fatal(e)
			}
			if !retry && e != sentinel {
				t.Fatal(e)
			}
		})
	}
}
func TestAuditFailureRollsBackEffect(t *testing.T) {
	s, m := fixture(t, 5)
	m.ExpectBegin()
	m.ExpectExec("UPDATE test").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("audit unavailable"))
	m.ExpectRollback()
	if e := s.Mutate(context.Background(), audit(), func(ctx context.Context, tx *sql.Tx) error { _, e := tx.ExecContext(ctx, "UPDATE test"); return e }); e == nil {
		t.Fatal("expected rollback")
	}
}
func TestAuditValidationBeforeBegin(t *testing.T) {
	s, _ := fixture(t, 5)
	for _, a := range []Audit{{}, {Actor: "a", Action: "x", Outcome: "ok", Inputs: []byte(`{"a":1,"a":2}`)}, {Actor: "a", Action: "x", Outcome: "ok", Inputs: []byte(`"` + strings.Repeat("x", 65536) + `"`)}} {
		if e := s.Mutate(context.Background(), a, func(context.Context, *sql.Tx) error { t.Fatal("ran invalid mutation"); return nil }); e == nil {
			t.Fatal("expected refusal")
		}
	}
}
func TestMigrationNewerAndIdempotent(t *testing.T) {
	for _, version := range []int{SchemaVersion, SchemaVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s, m := fixture(t, 5)
			m.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(version))
			e := s.Migrate(context.Background())
			if (e != nil) != (version > SchemaVersion) {
				t.Fatal(e)
			}
		})
	}
}
func TestMigrationCreatesEveryFamily(t *testing.T) {
	s, m := fixture(t, 5)
	m.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))
	m.ExpectBegin()
	for range SchemaStatements {
		m.ExpectExec("CREATE TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectBegin()
	for _, statement := range Migration2 {
		m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WithArgs(2).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectBegin()
	for _, statement := range Migration3 {
		m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WithArgs(3).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectBegin()
	for _, statement := range Migration4 {
		m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WithArgs(4).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectBegin()
	for _, statement := range Migration5 {
		m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WithArgs(5).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectBegin()
	for _, statement := range Migration6 {
		m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WithArgs(6).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	m.ExpectBegin()
	for _, statement := range Migration7 {
		m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectExec("INSERT INTO migrations").WithArgs(7).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	expectMigration8(m)
	expectMigration9(m)
	expectMigration10(m)
	expectMigration11(m)

	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(SchemaStatements) != 23 {
		t.Fatalf("schema families %d", len(SchemaStatements))
	}
}
func TestBlobMegabyteRoundtrip(t *testing.T) {
	s, m := fixture(t, 5)
	data := bytes.Repeat([]byte("abcd"), 262144)
	ref := BlobRef{Digest(data), int64(len(data))}
	chunks := (len(data) + 65534) / 65535
	m.ExpectBegin()
	m.ExpectQuery("SELECT size FROM blobs").WithArgs(ref.Digest).WillReturnRows(sqlmock.NewRows([]string{"size"}))
	for i := 0; i < chunks; i++ {
		m.ExpectExec("INSERT INTO blob_chunks").WithArgs(ref.Digest, i, data[i*65535:min((i+1)*65535, len(data))]).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	m.ExpectExec("INSERT INTO blobs").WithArgs(ref.Digest, len(data), chunks).WillReturnResult(sqlmock.NewResult(0, 1))
	expectAudit(m)
	m.ExpectCommit()
	got, e := s.PutBlob(context.Background(), audit(), data)
	if e != nil || got != ref {
		t.Fatalf("%v %v", got, e)
	}
	m.ExpectQuery("SELECT size,chunks").WithArgs(ref.Digest).WillReturnRows(sqlmock.NewRows([]string{"size", "chunks"}).AddRow(len(data), chunks))
	rows := sqlmock.NewRows([]string{"chunk_index", "data"})
	for i := 0; i < chunks; i++ {
		rows.AddRow(i, data[i*65535:min((i+1)*65535, len(data))])
	}
	m.ExpectQuery("SELECT chunk_index,data").WillReturnRows(rows)
	b, e := s.GetBlob(context.Background(), ref)
	if e != nil || !bytes.Equal(b, data) {
		t.Fatal(e)
	}
}
func TestBlobIntegrityFailure(t *testing.T) {
	s, m := fixture(t, 5)
	m.ExpectQuery("SELECT size,chunks").WillReturnRows(sqlmock.NewRows([]string{"size", "chunks"}).AddRow(3, 1))
	m.ExpectQuery("SELECT chunk_index,data").WillReturnRows(sqlmock.NewRows([]string{"chunk_index", "data"}).AddRow(0, []byte("bad")))
	if _, e := s.GetBlob(context.Background(), BlobRef{Digest([]byte("yes")), 3}); e == nil {
		t.Fatal("tamper accepted")
	}
}
func TestCanonicalNumbersAndOrder(t *testing.T) {
	want := `{"a":[1,0,0.01],"b":1000}`
	for _, raw := range []string{`{"b":1e3,"a":[1.00,-0,1e-2]}`, `{"a":[1,0,0.0100],"b":1000.0}`} {
		b, e := CanonicalJSON([]byte(raw))
		if e != nil || string(b) != want {
			t.Fatalf("%s %v", b, e)
		}
	}
	for _, raw := range []string{`{"x":1,"x":2}`, `{} {}`, `[`, `1e1000000`} {
		if _, e := CanonicalJSON([]byte(raw)); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestPortStableAndRefusals(t *testing.T) {
	a, e := Port("/tmp/fae", 0)
	b, _ := Port("/tmp/fae", 0)
	if e != nil || a != b || a < 3658 || a >= 3658+4096 {
		t.Fatalf("%d %d %v", a, b, e)
	}
	for _, p := range []string{"relative", "/tmp/../a", "/tmp//a", "/tmp/a/"} {
		if _, e := Port(p, 0); e == nil {
			t.Fatal(p)
		}
	}
	if _, e := Port("/tmp/a", 80); e == nil {
		t.Fatal("low port")
	}
}
func FuzzCanonicalIdempotent(f *testing.F) {
	for _, seed := range []string{`{"a":1.0}`, `[null,true,"a",1e-2]`, `{"a":{"x":1},"b":2}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		canonical, e := CanonicalJSON(b)
		if e != nil {
			return
		}
		again, e := CanonicalJSON(canonical)
		if e != nil || !bytes.Equal(canonical, again) {
			t.Fatalf("not idempotent %q -> %q: %v", canonical, again, e)
		}
	})
}
