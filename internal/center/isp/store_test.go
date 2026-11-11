package isp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestEnsureAndLink(t *testing.T) {
	db := newFake()
	e, err := Ensure(context.Background(), db, 0, "x")
	if err != nil || e != nil {
		t.Fatal("asn 0 must skip")
	}
	e, err = Ensure(context.Background(), db, 7552, "Viettel Group")
	if err != nil || e == nil || e.Name != "Viettel" {
		t.Fatalf("create %#v %v", e, err)
	}
	again, err := Ensure(context.Background(), db, 7552, "Viettel Group Corp")
	if err != nil || again.Name != "Viettel" || again.Organisation != "Viettel Group Corp" {
		t.Fatalf("update %#v %v", again, err)
	}
	linked, err := LinkAddress(context.Background(), db, "203.0.113.1", 7552, "Viettel Group")
	if err != nil || linked == nil || db.addrISP["203.0.113.1"] == "" {
		t.Fatal("link address", err)
	}
	if err = LinkResult(context.Background(), db, "r1", "n1", "203.0.113.1"); err != nil {
		t.Fatal(err)
	}
	if err = LinkResult(context.Background(), db, "r1", "n1", ""); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSkipsASNOutsideUint32(t *testing.T) {
	if uint64(^uint(0)) <= uint64(maxASN) {
		t.Skip("uint cannot represent an out-of-range ASN on this architecture")
	}
	e, err := Ensure(context.Background(), newFake(), uint(uint64(maxASN)+1), "invalid")
	if err != nil || e != nil {
		t.Fatalf("out-of-range ASN should be ignored: %#v %v", e, err)
	}
}

func TestEnsureErrors(t *testing.T) {
	db := newFake()
	db.queryErr = errors.New("boom")
	if _, err := Ensure(context.Background(), db, 1, "Org"); err == nil {
		t.Fatal("expected query error")
	}
	db = newFake()
	db.execErr = errors.New("write")
	if _, err := LinkAddress(context.Background(), db, "203.0.113.1", 1, "Org"); err == nil {
		t.Fatal("expected address lock error")
	}
}

func TestStoreListGetCompareTrendPlans(t *testing.T) {
	db := newFake()
	db.put(&Entity{ID: "00000000-0000-4000-8000-000000000001", ASN: 7552, Organisation: "Viettel Group", Name: "Viettel"})
	avg := 200.0
	db.listRows = [][]any{
		{"00000000-0000-4000-8000-000000000001", uint(7552), "Viettel Group", "Viettel", 1, avg, avg, 80.0, 1.0, 10.0},
	}
	db.trendRows = [][]any{{mustTime(), avg, 50.0, 10.0, 2}}
	store := &Store{DB: db, Now: mustTime}
	q := Query{From: mustTime().Add(-time.Hour), To: mustTime().Add(time.Hour)}
	list, err := store.List(context.Background(), q)
	if err != nil || len(list) != 1 || list[0]["name"] != "Viettel" {
		t.Fatalf("list %#v %v", list, err)
	}
	q.Engine = "ookla"
	if _, err = store.List(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "00000000-0000-4000-8000-000000000001", q)
	if err != nil || got["asn"].(uint) != 7552 {
		t.Fatalf("get %#v %v", got, err)
	}
	if _, err = store.Get(context.Background(), "bad", q); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	trend, err := store.Trend(context.Background(), "00000000-0000-4000-8000-000000000001", q)
	if err != nil || len(trend) != 1 {
		t.Fatalf("trend %#v %v", trend, err)
	}
	cmp, err := store.Compare(context.Background(), Query{From: q.From, To: q.To, ISPIDs: []string{"00000000-0000-4000-8000-000000000001"}})
	if err != nil || len(cmp) != 1 {
		t.Fatalf("compare %#v %v", cmp, err)
	}
	if _, err = store.Compare(context.Background(), Query{ISPIDs: []string{"bad"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	empty, err := store.Compare(context.Background(), Query{})
	if err != nil || len(empty) != 0 {
		t.Fatal(err)
	}
	plan, err := store.CreatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", "Fast", 300, 100)
	if err != nil {
		t.Fatal(err)
	}
	id := plan["id"].(string)
	if _, err = store.UpdatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", id, "Fast", 350, 100); err != nil {
		t.Fatal(err)
	}
	plans, err := store.ListPlans(context.Background(), "00000000-0000-4000-8000-000000000001")
	if err != nil || len(plans) != 1 {
		t.Fatal(err)
	}
	if err = store.DeletePlan(context.Background(), "00000000-0000-4000-8000-000000000001", id); err != nil {
		t.Fatal(err)
	}
	if _, err = ByID(context.Background(), db, "00000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreValidationPaths(t *testing.T) {
	db := newFake()
	store := &Store{DB: db}
	_ = store.now()
	if _, err := store.CreatePlan(context.Background(), "bad", "x", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	db.exists = false
	if _, err := store.CreatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", "x", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	db.exists = true
	db.put(&Entity{ID: "00000000-0000-4000-8000-000000000001", ASN: 1, Organisation: "O", Name: "O"})
	if _, err := store.CreatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", " ", 1, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := store.DeletePlan(context.Background(), "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := store.Trend(context.Background(), "00000000-0000-4000-8000-000000000099", Query{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := ByID(context.Background(), db, "bad"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := ByID(context.Background(), db, "00000000-0000-4000-8000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	db.execErr = &pgconn.PgError{Code: "23505"}
	if _, err := store.CreatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", "Dup", 10, 10); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	db.execErr = nil
	plan, err := store.CreatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", "Plan", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	db.execErr = &pgconn.PgError{Code: "23505"}
	if _, err = store.UpdatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", plan["id"].(string), "Plan", 11, 11); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	db.execErr = nil
	if _, err = store.UpdatePlan(context.Background(), "00000000-0000-4000-8000-000000000001", plan["id"].(string), " ", 1, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = store.UpdatePlan(context.Background(), "bad", "bad", "x", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	db.queryErr = errors.New("q")
	if _, err = store.ListPlans(context.Background(), "00000000-0000-4000-8000-000000000001"); err == nil {
		t.Fatal("expected list plans error")
	}
	db.queryErr = nil
	if linked, err := LinkAddress(context.Background(), db, "203.0.113.9", 0, ""); err != nil || linked != nil {
		t.Fatal("skip empty asn")
	}
	if !isUnique(&pgconn.PgError{Code: "23505"}) || isUnique(errors.New("x")) {
		t.Fatal("isUnique")
	}
	db.execErr = errors.New("del")
	if err := store.DeletePlan(context.Background(), "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000001"); err == nil {
		t.Fatal("expected delete error")
	}
	db.execErr = nil
	db.queryErr = errors.New("trend")
	if _, err := store.Trend(context.Background(), "00000000-0000-4000-8000-000000000001", Query{}); err == nil {
		t.Fatal("expected trend error")
	}
	db.queryErr = nil
	if item, err := store.getPlan(context.Background(), "missing"); !errors.Is(err, ErrNotFound) || item != nil {
		t.Fatal(err)
	}
}
