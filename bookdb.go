package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	TypeBytes = 1
	TypeInt64 = 2
)

type BVal struct {
	Type uint32
	I64  int64
	Str  []byte
}

type Record struct {
	Cols []string
	Vals []BVal
}

func (r *Record) AddStr(c string, v []byte) *Record {
	r.Cols = append(r.Cols, c)
	r.Vals = append(r.Vals, BVal{Type: TypeBytes, Str: v})
	return r
}
func (r *Record) AddInt64(c string, v int64) *Record {
	r.Cols = append(r.Cols, c)
	r.Vals = append(r.Vals, BVal{Type: TypeInt64, I64: v})
	return r
}
func (r *Record) Get(c string) *BVal {
	for i, n := range r.Cols {
		if n == c {
			return &r.Vals[i]
		}
	}
	return nil
}

// BTableDef: Indexes[0] is the primary key; Prefixes[i] belongs to Indexes[i].
type BTableDef struct {
	Name     string
	Types    []uint32
	Cols     []string
	Indexes  [][]string
	Prefixes []uint32
}

var tdefMeta = &BTableDef{Name: "@meta", Types: []uint32{TypeBytes, TypeBytes}, Cols: []string{"key", "val"},
	Indexes: [][]string{{"key"}}, Prefixes: []uint32{1}}
var tdefTable = &BTableDef{Name: "@table", Types: []uint32{TypeBytes, TypeBytes}, Cols: []string{"name", "def"},
	Indexes: [][]string{{"name"}}, Prefixes: []uint32{2}}

const tablePrefixMin = 3

// ---------------------------------------------------------------- encoding
func escapeString(in []byte) []byte {
	var out []byte
	for _, b := range in {
		switch b {
		case 0:
			out = append(out, 1, 1)
		case 1:
			out = append(out, 1, 2)
		default:
			out = append(out, b)
		}
	}
	return out
}

func unescapeString(in []byte) []byte {
	var out []byte
	for i := 0; i < len(in); i++ {
		if in[i] == 1 && i+1 < len(in) {
			i++
			out = append(out, in[i]-1)
		} else {
			out = append(out, in[i])
		}
	}
	return out
}

func encodeValues(out []byte, vals []BVal) []byte {
	for _, v := range vals {
		out = append(out, byte(v.Type)) // tag: never starts with 0xff
		switch v.Type {
		case TypeInt64:
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(v.I64)+(1<<63))
			out = append(out, b[:]...)
		case TypeBytes:
			out = append(out, escapeString(v.Str)...)
			out = append(out, 0)
		default:
			panic("bad type")
		}
	}
	return out
}

func decodeValues(in []byte, out []BVal) {
	for i := range out {
		in = in[1:]
		switch out[i].Type {
		case TypeInt64:
			out[i].I64 = int64(binary.BigEndian.Uint64(in[:8]) - (1 << 63))
			in = in[8:]
		case TypeBytes:
			n := bytes.IndexByte(in, 0)
			out[i].Str = unescapeString(in[:n])
			in = in[n+1:]
		}
	}
}

func encodeKey(out []byte, prefix uint32, vals []BVal) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], prefix)
	return encodeValues(append(out, b[:]...), vals)
}

const (
	CmpGE = 3
	CmpGT = 2
	CmpLT = -2
	CmpLE = -3
)

// missing trailing columns: +infinity = 0xff for GT/LE, -infinity = "" otherwise.
func encodeKeyPartial(out []byte, prefix uint32, vals []BVal, cmp int) []byte {
	out = encodeKey(out, prefix, vals)
	if cmp == CmpGT || cmp == CmpLE {
		out = append(out, 0xff)
	}
	return out
}

// ---------------------------------------------------------------- schema helpers
func (t *BTableDef) col(name string) int {
	for i, c := range t.Cols {
		if c == name {
			return i
		}
	}
	return -1
}

// fullIndex = index columns + any primary-key columns not already in it.
func (t *BTableDef) fullIndex(i int) []string {
	idx := append([]string(nil), t.Indexes[i]...)
	if i == 0 {
		return idx
	}
	for _, pk := range t.Indexes[0] {
		found := false
		for _, c := range idx {
			if c == pk {
				found = true
			}
		}
		if !found {
			idx = append(idx, pk)
		}
	}
	return idx
}

func (t *BTableDef) isPK(c string) bool {
	for _, p := range t.Indexes[0] {
		if p == c {
			return true
		}
	}
	return false
}

// checkRecord reorders rec to schema order. full=true: every column required;
// otherwise exactly the primary-key columns.
func checkRecord(t *BTableDef, rec Record, full bool) ([]BVal, error) {
	vals := make([]BVal, len(t.Cols))
	seen := make([]bool, len(t.Cols))
	for i, c := range rec.Cols {
		j := t.col(c)
		if j < 0 {
			return nil, fmt.Errorf("unknown column: %s", c)
		}
		if rec.Vals[i].Type != t.Types[j] {
			return nil, fmt.Errorf("bad column type: %s", c)
		}
		vals[j], seen[j] = rec.Vals[i], true
	}
	for j, c := range t.Cols {
		need := full || t.isPK(c)
		if need && !seen[j] {
			return nil, fmt.Errorf("missing column: %s", c)
		}
		if !need && seen[j] {
			return nil, fmt.Errorf("extra column: %s", c)
		}
		vals[j].Type = t.Types[j]
	}
	return vals, nil
}

func (t *BTableDef) pick(vals []BVal, cols []string) []BVal {
	out := make([]BVal, len(cols))
	for i, c := range cols {
		out[i] = vals[t.col(c)]
	}
	return out
}

func (t *BTableDef) nonPK() []string {
	var out []string
	for _, c := range t.Cols {
		if !t.isPK(c) {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------- DB + TX
type BDB struct{ kv *KV }

func OpenBDB(path string) (*BDB, error) {
	kv, err := OpenKV(path)
	if err != nil {
		return nil, err
	}
	return &BDB{kv: kv}, nil
}
func (db *BDB) Close() error { return db.kv.Close() }

type DBTX struct {
	db *BDB
	tx *KVTX
}

func (db *BDB) Begin() *DBTX     { return &DBTX{db: db, tx: db.kv.Begin()} }
func (t *DBTX) Commit() error    { return t.tx.Commit() }
func (t *DBTX) Abort()           { t.tx.Abort() }

func (t *DBTX) wrap(f func()) (err error) {
	defer catchDBError(&err)
	f()
	return
}

func (t *DBTX) getTableDef(name string) *BTableDef {
	switch name {
	case "@meta":
		return tdefMeta
	case "@table":
		return tdefTable
	}
	rec := (&Record{}).AddStr("name", []byte(name))
	ok, err := t.dbGet(tdefTable, rec)
	if err != nil || !ok {
		return nil
	}
	td := &BTableDef{}
	if json.Unmarshal(rec.Get("def").Str, td) != nil {
		return nil
	}
	return td
}

func (t *DBTX) dbGet(td *BTableDef, rec *Record) (bool, error) {
	vals, err := checkRecord(td, *rec, false)
	if err != nil {
		return false, err
	}
	key := encodeKey(nil, td.Prefixes[0], td.pick(vals, td.Indexes[0]))
	v, ok := t.tx.Get(string(key))
	if !ok {
		return false, nil
	}
	np := td.nonPK()
	out := td.pick(vals, np)
	decodeValues([]byte(v), out)
	for i, c := range np {
		vals[td.col(c)] = out[i]
	}
	rec.Cols, rec.Vals = td.Cols, vals
	return true, nil
}

func (t *DBTX) dbUpdate(td *BTableDef, rec Record, mode int) (bool, error) {
	vals, err := checkRecord(td, rec, true)
	if err != nil {
		return false, err
	}
	key := encodeKey(nil, td.Prefixes[0], td.pick(vals, td.Indexes[0]))
	val := encodeValues(nil, td.pick(vals, td.nonPK()))
	req := UpdateReq{Key: string(key), Val: string(val), Mode: mode}
	if err = t.wrap(func() { t.tx.Update(&req) }); err != nil {
		return false, err
	}
	if !req.Updated || len(td.Indexes) == 1 {
		return req.Updated, nil
	}
	err = t.wrap(func() {
		if !req.Added { // remove the index keys of the old row
			old := td.pick(vals, td.nonPK())
			decodeValues([]byte(req.Old), old)
			oldVals := append([]BVal(nil), vals...)
			for i, c := range td.nonPK() {
				oldVals[td.col(c)] = old[i]
			}
			for i := 1; i < len(td.Indexes); i++ {
				t.tx.Del(string(encodeKey(nil, td.Prefixes[i], td.pick(oldVals, td.fullIndex(i)))))
			}
		}
		for i := 1; i < len(td.Indexes); i++ {
			t.tx.Set(string(encodeKey(nil, td.Prefixes[i], td.pick(vals, td.fullIndex(i)))), "")
		}
	})
	return req.Updated, err
}

func (t *DBTX) dbDelete(td *BTableDef, rec Record) (bool, error) {
	row := rec
	ok, err := t.dbGet(td, &row)
	if err != nil || !ok {
		return false, err
	}
	err = t.wrap(func() {
		t.tx.Del(string(encodeKey(nil, td.Prefixes[0], td.pick(row.Vals, td.Indexes[0]))))
		for i := 1; i < len(td.Indexes); i++ {
			t.tx.Del(string(encodeKey(nil, td.Prefixes[i], td.pick(row.Vals, td.fullIndex(i)))))
		}
	})
	return true, err
}

func (t *DBTX) need(table string) (*BTableDef, error) {
	td := t.getTableDef(table)
	if td == nil {
		return nil, fmt.Errorf("table not found: %s", table)
	}
	return td, nil
}

func (t *DBTX) Get(table string, rec *Record) (bool, error) {
	td, err := t.need(table)
	if err != nil {
		return false, err
	}
	return t.dbGet(td, rec)
}
func (t *DBTX) mut(table string, rec Record, mode int) (bool, error) {
	td, err := t.need(table)
	if err != nil {
		return false, err
	}
	return t.dbUpdate(td, rec, mode)
}
func (t *DBTX) Insert(table string, rec Record) (bool, error) { return t.mut(table, rec, ModeInsertOnly) }
func (t *DBTX) Update(table string, rec Record) (bool, error) { return t.mut(table, rec, ModeUpdateOnly) }
func (t *DBTX) Upsert(table string, rec Record) (bool, error) { return t.mut(table, rec, ModeUpsert) }
func (t *DBTX) Delete(table string, rec Record) (bool, error) {
	td, err := t.need(table)
	if err != nil {
		return false, err
	}
	return t.dbDelete(td, rec)
}

// TableNew: read @table (duplicate check), bump the prefix counter in @meta,
// store the schema in @table - atomically, because it is one KV transaction.
func (t *DBTX) TableNew(td *BTableDef) error {
	if len(td.Cols) == 0 || len(td.Cols) != len(td.Types) || len(td.Indexes) == 0 || len(td.Indexes[0]) == 0 {
		return errors.New("bad table definition")
	}
	for _, ix := range td.Indexes {
		for _, c := range ix {
			if td.col(c) < 0 {
				return fmt.Errorf("index column not found: %s", c)
			}
		}
	}
	if t.getTableDef(td.Name) != nil || td.Name == "" || td.Name[0] == '@' {
		return fmt.Errorf("table exists or bad name: %s", td.Name)
	}
	meta := (&Record{}).AddStr("key", []byte("next_prefix"))
	ok, err := t.dbGet(tdefMeta, meta)
	if err != nil {
		return err
	}
	next := uint32(tablePrefixMin)
	if ok {
		next = binary.LittleEndian.Uint32(meta.Get("val").Str)
	}
	td.Prefixes = nil
	for range td.Indexes {
		td.Prefixes = append(td.Prefixes, next)
		next++
	}
	var nb [4]byte
	binary.LittleEndian.PutUint32(nb[:], next)
	if _, err = t.dbUpdate(tdefMeta, *(&Record{}).AddStr("key", []byte("next_prefix")).AddStr("val", nb[:]), ModeUpsert); err != nil {
		return err
	}
	js, _ := json.Marshal(td)
	_, err = t.dbUpdate(tdefTable, *(&Record{}).AddStr("name", []byte(td.Name)).AddStr("def", js), ModeInsertOnly)
	return err
}

// ---------------------------------------------------------------- range scans
type Scanner struct {
	Cmp1, Cmp2 int // CmpGE/CmpGT (ascending) or CmpLE/CmpLT (descending); Cmp2 == 0: open
	Key1, Key2 Record
	rows       []Record
	pos        int
}

func (s *Scanner) Valid() bool         { return s.pos < len(s.rows) }
func (s *Scanner) Next()               { s.pos++ }
func (s *Scanner) Deref(rec *Record)   { *rec = s.rows[s.pos] }

func covered(index, key []string) bool {
	if len(index) < len(key) {
		return false
	}
	for i := range key {
		if index[i] != key[i] {
			return false
		}
	}
	return true
}

func (t *DBTX) Scan(table string, req *Scanner) error {
	td, err := t.need(table)
	if err != nil {
		return err
	}
	if req.Cmp1 != CmpGE && req.Cmp1 != CmpGT && req.Cmp1 != CmpLE && req.Cmp1 != CmpLT {
		return errors.New("bad range")
	}
	if req.Cmp2 != 0 && (req.Cmp1 > 0) == (req.Cmp2 > 0) {
		return errors.New("bad range: Cmp1 and Cmp2 must point in opposite directions")
	}
	idx := -1
	for i := range td.Indexes {
		if covered(td.Indexes[i], req.Key1.Cols) && (req.Cmp2 == 0 || covered(td.Indexes[i], req.Key2.Cols)) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("no index found")
	}
	full := td.fullIndex(idx)
	keyVals := func(r Record) ([]BVal, error) {
		out := make([]BVal, len(r.Cols))
		for i, c := range r.Cols {
			j := td.col(c)
			if j < 0 || r.Vals[i].Type != td.Types[j] {
				return nil, fmt.Errorf("bad scan column: %s", c)
			}
			out[i] = r.Vals[i]
		}
		return out, nil
	}
	v1, err := keyVals(req.Key1)
	if err != nil {
		return err
	}
	pfx := td.Prefixes[idx]
	var b1, b2 []byte
	b1 = encodeKeyPartial(nil, pfx, v1, req.Cmp1)
	if req.Cmp2 != 0 {
		v2, err := keyVals(req.Key2)
		if err != nil {
			return err
		}
		b2 = encodeKeyPartial(nil, pfx, v2, req.Cmp2)
	}
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], pfx)
	start := append(append([]byte(nil), head[:]...), 0)
	start = start[:4]
	end := append(append([]byte(nil), head[:]...), 0xff)
	asc := req.Cmp1 > 0
	lo, hi := b1, b2
	loExcl, hiExcl := false, false
	if asc {
		if b2 == nil {
			hi = end
		}
		hiExcl = req.Cmp2 == CmpLT
	} else {
		lo, hi = b2, b1
		if b2 == nil {
			lo = start
		}
		hiExcl = req.Cmp1 == CmpLT
	}
	_ = loExcl
	req.rows, req.pos = nil, 0
	return t.wrap(func() {
		types := make([]BVal, len(full))
		for i, c := range full {
			types[i].Type = td.Types[td.col(c)]
		}
		t.tx.Scan(string(lo), string(hi), false, !asc, func(k, v string) bool {
			if hiExcl && k >= string(hi) {
				return !asc // asc: past the end -> stop; desc: skip equal key
			}
			kv := append([]BVal(nil), types...)
			decodeValues([]byte(k[4:]), kv)
			row := make([]BVal, len(td.Cols))
			for i, c := range full {
				row[td.col(c)] = kv[i]
			}
			np := td.nonPK()
			if idx == 0 {
				out := td.pick(row, np)
				for i, c := range np {
					out[i].Type = td.Types[td.col(c)]
				}
				decodeValues([]byte(v), out)
				for i, c := range np {
					row[td.col(c)] = out[i]
				}
			} else {
				pkRec := Record{}
				for _, c := range td.Indexes[0] {
					pkRec.Cols = append(pkRec.Cols, c)
					pkRec.Vals = append(pkRec.Vals, row[td.col(c)])
				}
				ok, err := t.dbGet(td, &pkRec)
				if err != nil || !ok {
					fail("corrupt index: entry without row")
				}
				row = pkRec.Vals
			}
			req.rows = append(req.rows, Record{Cols: td.Cols, Vals: row})
			return true
		})
	})
}
