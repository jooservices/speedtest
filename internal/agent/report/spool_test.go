package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openSpool(t *testing.T, cfg SpoolConfig) *Spool {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = privateDir(t)
	}
	s, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func result(t *testing.T) Result {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	r := okResult(id)
	r.TS = time.Now().UTC().Format(time.RFC3339)
	return r
}
func peek(t *testing.T, s *Spool, n int) []Result {
	t.Helper()
	rs, err := s.Peek(n)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}
func appendResult(t *testing.T, s *Spool, r Result) {
	t.Helper()
	if err := s.Append(r); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolFIFOAndRestart(t *testing.T) {
	dir := privateDir(t)
	s := openSpool(t, SpoolConfig{Dir: dir})
	a, b, c := result(t), result(t), result(t)
	for _, r := range []Result{a, b, c} {
		appendResult(t, s, r)
	}
	if err := s.Ack(b.ID); err == nil {
		t.Fatal("out of order ack accepted")
	}
	if err := s.Ack(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openSpool(t, SpoolConfig{Dir: dir})
	got := peek(t, s, 100)
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != c.ID {
		t.Fatal("restart lost FIFO", got)
	}
	*got[0].Down = 999
	if *peek(t, s, 1)[0].Down == 999 {
		t.Fatal("peek aliases stored value")
	}
	for _, r := range []Result{b, c} {
		if err := s.Ack(r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(peek(t, s, 1)) != 0 {
		t.Fatal("not drained")
	}
	if err := s.Ack(a.ID); err != nil {
		t.Fatal("ack already evicted item", err)
	}
	for _, p := range []string{dir, filepath.Join(dir, "spool.sqlite")} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("not private", err)
		}
	}
}

func TestSpoolDuplicateAndConflict(t *testing.T) {
	s := openSpool(t, SpoolConfig{})
	r := result(t)
	appendResult(t, s, r)
	appendResult(t, s, r)
	r.Threads++
	if err := s.Append(r); err == nil {
		t.Fatal("UUID conflict accepted")
	}
	if len(peek(t, s, 100)) != 1 {
		t.Fatal("duplicate retained")
	}
	if err := s.Append(Result{}); err == nil {
		t.Fatal("invalid result accepted")
	}
	if _, err := s.Peek(0); err == nil {
		t.Fatal("invalid batch")
	}
}

func TestSpoolCountBytesAndAgeCaps(t *testing.T) {
	for _, kind := range []string{"count", "bytes", "age"} {
		t.Run(kind, func(t *testing.T) {
			a, b, c := result(t), result(t), result(t)
			cfg := SpoolConfig{Dir: privateDir(t), MaxResults: 2}
			switch kind {
			case "bytes":
				data, _ := json.Marshal(a)
				cfg.MaxBytes = int64(2 * len(data))
				cfg.MaxResults = 100
			case "age":
				cfg.MaxAge = time.Hour
				cfg.MaxResults = 100
				a.TS = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			}
			logger, buf := testLogger(t)
			s, err := Open(cfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			for _, r := range []Result{a, b, c} {
				appendResult(t, s, r)
			}
			got := peek(t, s, 10)
			if len(got) != 2 || got[0].ID != b.ID || got[1].ID != c.ID {
				t.Fatal("cap did not keep newest", got)
			}
			if !strings.Contains(buf.String(), "dropped oldest unsent result") {
				t.Fatal("missing warning")
			}
			s.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
			if err := s.Prune(); err != nil {
				t.Fatal(err)
			}
			if len(peek(t, s, 10)) != 0 {
				t.Fatal("idle backlog not expired")
			}
		})
	}
}

func TestSpoolConcurrentProducers(t *testing.T) {
	s := openSpool(t, SpoolConfig{MaxResults: 10})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		r := result(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Append(r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(peek(t, s, 100)) != 10 {
		t.Fatal("count cap")
	}
}

func TestSpoolFailures(t *testing.T) {
	if _, err := Open(SpoolConfig{}, nil); err == nil {
		t.Fatal("empty dir")
	}
	dir := privateDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(SpoolConfig{Dir: filepath.Join(file, "child")}, nil); err == nil {
		t.Fatal("file dir")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(SpoolConfig{Dir: dir}, nil); err == nil {
		t.Fatal("public dir")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "spool.sqlite"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(SpoolConfig{Dir: dir}, nil); err == nil {
		t.Fatal("database is directory")
	}
	corrupt := privateDir(t)
	if err := os.WriteFile(filepath.Join(corrupt, "spool.sqlite"), []byte("bad database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(SpoolConfig{Dir: corrupt}, nil); err == nil {
		t.Fatal("corrupt database")
	}
	s := openSpool(t, SpoolConfig{})
	r := result(t)
	if _, err := s.db.Exec(`INSERT INTO pending(id,ts,payload) VALUES(?,?,?)`, r.ID, time.Now().UnixNano(), "bad"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Peek(1); err == nil {
		t.Fatal("corrupt payload")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(r); err == nil {
		t.Fatal("closed append")
	}
	if err := s.Prune(); err == nil {
		t.Fatal("closed prune")
	}
	if _, err := s.Peek(1); err == nil {
		t.Fatal("closed peek")
	}
	if err := s.Ack(r.ID); err == nil {
		t.Fatal("closed ack")
	}
	if err := s.Quarantine(r.ID, "HTTP 422"); err == nil {
		t.Fatal("closed quarantine")
	}
}

func TestSpoolRestartAppliesCaps(t *testing.T) {
	dir := privateDir(t)
	s := openSpool(t, SpoolConfig{Dir: dir})
	a, b := result(t), result(t)
	appendResult(t, s, a)
	appendResult(t, s, b)
	_ = s.Close()
	s = openSpool(t, SpoolConfig{Dir: dir, MaxResults: 1})
	if got := peek(t, s, 2); len(got) != 1 || got[0].ID != b.ID {
		t.Fatal(fmt.Sprint(got))
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSpoolCapsOutOfOrderTimestamps(t *testing.T) {
	s := openSpool(t, SpoolConfig{MaxAge: time.Hour})
	a, b := result(t), result(t)
	b.TS = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	appendResult(t, s, a)
	appendResult(t, s, b)
	if len(peek(t, s, 100)) != 0 {
		t.Fatal("age cap ignored a stale result behind fresh head")
	}
}

func TestQuarantineBoundsAndOrdering(t *testing.T) {
	s := openSpool(t, SpoolConfig{MaxResults: 1})
	a, b := result(t), result(t)
	appendResult(t, s, a)
	if err := s.Quarantine(a.ID, "HTTP 422"); err != nil {
		t.Fatal(err)
	}
	appendResult(t, s, b)
	if err := s.Quarantine(b.ID, "HTTP 409"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM quarantine`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.Quarantine(a.ID, "HTTP 422"); err != nil {
		t.Fatal(err)
	}
	// Wrong-order quarantine must leave both pending rows untouched.
	s.cfg.MaxResults = 10
	appendResult(t, s, a)
	appendResult(t, s, b)
	if err := s.Quarantine(b.ID, "HTTP 422"); err == nil {
		t.Fatal("out of order quarantine")
	}
}

func TestSQLiteFailureTransactionsKeepPending(t *testing.T) {
	for _, op := range []string{"append", "ack", "quarantine", "prune"} {
		t.Run(op, func(t *testing.T) {
			s := openSpool(t, SpoolConfig{})
			a := result(t)
			appendResult(t, s, a)
			var err error
			switch op {
			case "append":
				_, err = s.db.Exec(`CREATE TRIGGER fail BEFORE INSERT ON pending BEGIN SELECT RAISE(ABORT,'fixture'); END`)
				if err == nil {
					err = s.Append(result(t))
				}
			case "ack", "prune":
				_, err = s.db.Exec(`CREATE TRIGGER fail BEFORE DELETE ON pending BEGIN SELECT RAISE(ABORT,'fixture'); END`)
				if err == nil {
					if op == "ack" {
						err = s.Ack(a.ID)
					} else {
						s.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
						err = s.Prune()
					}
				}
			case "quarantine":
				_, err = s.db.Exec(`CREATE TRIGGER fail BEFORE DELETE ON pending BEGIN SELECT RAISE(ABORT,'fixture'); END`)
				if err == nil {
					err = s.Quarantine(a.ID, "HTTP 422")
				}
			}
			if err == nil {
				t.Fatal("failure not propagated")
			}
			if got := peek(t, s, 10); len(got) != 1 || got[0].ID != a.ID {
				t.Fatal("transaction lost pending result")
			}
		})
	}
}
