package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func tmp(t *testing.T) string { return filepath.Join(t.TempDir(), "t.db") }

func TestBTreeBackward(t *testing.T) {
	pg, _ := OpenPager(tmp(t))
	bt := NewBTree(pg)
	for i := 0; i < 2000; i++ {
		bt.Set(fmt.Sprintf("k%05d", i*2), "v")
	}
	pg.Commit()
	c := bt.SeekLE("k00101")
	if !c.Valid() || c.Key() != "k00100" {
		t.Fatalf("SeekLE got %v", c.Key())
	}
	n := 0
	for ; c.Valid(); c.Prev() {
		n++
	}
	if n != 51 {
		t.Fatalf("prev walk %d", n)
	}
	if bt.Last().Key() != "k03998" {
		t.Fatal("Last")
	}
	var req UpdateReq = UpdateReq{Key: "k00004", Val: "x", Mode: ModeInsertOnly}
	bt.Update(&req)
	if req.Updated {
		t.Fatal("insert-only overwrote")
	}
	if bt.Verify() == 0 {
		t.Fatal("verify")
	}
}

func TestKVSnapshotAndConflict(t *testing.T) {
	kv, _ := OpenKV(tmp(t))
	w := kv.Begin()
	w.Set("a", "1")
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	r := kv.Begin() // snapshot sees a=1
	w2 := kv.Begin()
	w2.Set("a", "2")
	w2.Set("b", "x")
	if err := w2.Commit(); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Get("a"); v != "1" {
		t.Fatalf("snapshot isolation broken: %s", v)
	}
	r.Abort()
	// write-write / read-write conflict
	x, y := kv.Begin(), kv.Begin()
	x.Get("a")
	x.Set("a", "X")
	y.Get("a")
	y.Set("a", "Y")
	if err := x.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := y.Commit(); err != ErrConflict {
		t.Fatalf("want conflict, got %v", err)
	}
	// disjoint keys: no conflict
	p, q := kv.Begin(), kv.Begin()
	p.Set("p", "1")
	q.Set("q", "1")
	if p.Commit() != nil || q.Commit() != nil {
		t.Fatal("disjoint writers must both commit")
	}
	// range read vs insert into the range (phantom)
	s, ins := kv.Begin(), kv.Begin()
	s.Scan("a", "z", false, false, func(k, v string) bool { return true })
	s.Set("zz", "1")
	ins.Set("m", "1")
	ins.Commit()
	if s.Commit() != ErrConflict {
		t.Fatal("phantom not detected")
	}
	// reader pinned across many commits must still read a valid snapshot
	rd := kv.Begin()
	for i := 0; i < 300; i++ {
		tx := kv.Begin()
		tx.Set(fmt.Sprintf("n%04d", i), "v")
		tx.Del("a")
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	cnt := 0
	rd.Scan("", "", true, false, func(k, v string) bool { cnt++; return true })
	if cnt != 4 { // a, b, p, q + m  -> a b m p q = 5? snapshot taken after m
		if cnt != 5 {
			t.Fatalf("pinned snapshot corrupted: %d", cnt)
		}
	}
	rd.Abort()
	kv.Close()
}

func TestDurabilityReopen(t *testing.T) {
	path := tmp(t)
	db, _ := OpenBDB(path)
	tx := db.Begin()
	tx.TableNew(&BTableDef{Name: "t", Types: []uint32{TypeInt64, TypeBytes}, Cols: []string{"id", "v"}, Indexes: [][]string{{"id"}}})
	tx.Insert("t", *(&Record{}).AddInt64("id", 1).AddStr("v", []byte("one")))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, _ = OpenBDB(path)
	tx = db.Begin()
	r := (&Record{}).AddInt64("id", 1)
	ok, _ := tx.Get("t", r)
	if !ok || string(r.Get("v").Str) != "one" {
		t.Fatal("lost after reopen")
	}
}

func TestRelationalAPI(t *testing.T) {
	db, _ := OpenBDB(tmp(t))
	tx := db.Begin()
	err := tx.TableNew(&BTableDef{Name: "t1",
		Types:   []uint32{TypeBytes, TypeInt64, TypeBytes, TypeBytes},
		Cols:    []string{"k1", "k2", "v1", "v2"},
		Indexes: [][]string{{"k1", "k2"}, {"v1"}, {"v2", "k2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if tx.TableNew(&BTableDef{Name: "t1", Types: []uint32{TypeBytes}, Cols: []string{"a"}, Indexes: [][]string{{"a"}}}) == nil {
		t.Fatal("duplicate table accepted")
	}
	rec := func(k1 string, k2 int64, v1, v2 string) Record {
		return *(&Record{}).AddStr("k1", []byte(k1)).AddInt64("k2", k2).AddStr("v1", []byte(v1)).AddStr("v2", []byte(v2))
	}
	for i := int64(0); i < 300; i++ {
		ok, err := tx.Insert("t1", rec("k", i-100, fmt.Sprintf("a%03d", i%10), fmt.Sprintf("b%03d", i)))
		if !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if ok, _ := tx.Insert("t1", rec("k", 5, "x", "y")); ok {
		t.Fatal("duplicate PK inserted")
	}
	if ok, _ := tx.Update("t1", rec("nope", 1, "x", "y")); ok {
		t.Fatal("update of missing row")
	}
	if ok, _ := tx.Upsert("t1", rec("k", 5, "zzz", "yyy")); !ok { // moves secondary index entries
		t.Fatal("upsert")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = db.Begin()
	count := func(s *Scanner) (n int, first, last Record) {
		if err := tx.Scan("t1", s); err != nil {
			t.Fatal(err)
		}
		for ; s.Valid(); s.Next() {
			var r Record
			s.Deref(&r)
			if n == 0 {
				first = r
			}
			last = r
			n++
		}
		return
	}
	// primary key range, negative ints must sort correctly
	s := &Scanner{Cmp1: CmpGE, Key1: *(&Record{}).AddStr("k1", []byte("k")).AddInt64("k2", -100), Cmp2: CmpLT, Key2: *(&Record{}).AddStr("k1", []byte("k")).AddInt64("k2", 0)}
	if n, f, _ := count(s); n != 100 || f.Vals[1].I64 != -100 {
		t.Fatalf("pk range: %d %v", n, f.Vals[1].I64)
	}
	// descending
	s = &Scanner{Cmp1: CmpLE, Key1: *(&Record{}).AddStr("k1", []byte("k")).AddInt64("k2", 10), Cmp2: CmpGT, Key2: *(&Record{}).AddStr("k1", []byte("k")).AddInt64("k2", 5)}
	if n, f, l := count(s); n != 5 || f.Vals[1].I64 != 10 || l.Vals[1].I64 != 6 {
		t.Fatalf("desc: %d %d %d", n, f.Vals[1].I64, l.Vals[1].I64)
	}
	// prefix-only key on the primary index (missing column = infinity)
	s = &Scanner{Cmp1: CmpGT, Key1: *(&Record{}).AddStr("k1", []byte("j"))}
	if n, _, _ := count(s); n != 300 {
		t.Fatalf("prefix GT: %d", n)
	}
	// secondary index: v1 == a003 (30 rows), upsert moved row k2=5 (v1 a005 -> zzz)
	s = &Scanner{Cmp1: CmpGE, Key1: *(&Record{}).AddStr("v1", []byte("a003")), Cmp2: CmpLE, Key2: *(&Record{}).AddStr("v1", []byte("a003"))}
	if n, _, _ := count(s); n != 30 {
		t.Fatalf("idx v1: %d", n)
	}
	s = &Scanner{Cmp1: CmpGE, Key1: *(&Record{}).AddStr("v1", []byte("a005")), Cmp2: CmpLE, Key2: *(&Record{}).AddStr("v1", []byte("a005"))}
	if n, _, _ := count(s); n != 29 {
		t.Fatalf("stale index entry after upsert: %d", n)
	}
	// delete removes index entries too
	if ok, _ := tx.Delete("t1", *(&Record{}).AddStr("k1", []byte("k")).AddInt64("k2", 5)); !ok {
		t.Fatal("delete")
	}
	s = &Scanner{Cmp1: CmpGE, Key1: *(&Record{}).AddStr("v1", []byte("zzz"))}
	if n, _, _ := count(s); n != 0 {
		t.Fatalf("index entry survived delete: %d", n)
	}
	// reading own writes through the merged iterator + no index covers -> error
	if tx.Scan("t1", &Scanner{Cmp1: CmpGE, Key1: *(&Record{}).AddStr("k2", nil)}) == nil {
		t.Fatal("expected no-index error")
	}
	tx.Abort()
}
