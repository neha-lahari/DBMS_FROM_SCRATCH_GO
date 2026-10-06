package main

import (
	"errors"
	"sort"
	"sync"
)

var ErrConflict = errors.New("transaction conflict: a concurrent transaction changed data this transaction read")

type keyRange struct {
	lo, hi string
	hiInf  bool
}

func (r keyRange) has(k string) bool { return k >= r.lo && (r.hiInf || k <= r.hi) }

type committedTX struct {
	version uint64
	writes  []string
}

type KV struct {
	mu      sync.Mutex
	pg      *Pager
	version uint64
	history []committedTX
	active  map[*KVTX]struct{}
}

func OpenKV(path string) (*KV, error) {
	pg, err := OpenPager(path)
	if err != nil {
		return nil, err
	}
	return &KV{pg: pg, active: map[*KVTX]struct{}{}}, nil
}

func (kv *KV) Close() error { kv.mu.Lock(); defer kv.mu.Unlock(); return kv.pg.Close() }

type pend struct {
	val string
	del bool
}

type KVTX struct {
	kv      *KV
	tree    *BTree // snapshot tree
	version uint64
	pending map[string]pend
	keys    []string // sorted keys of pending
	reads   []keyRange
	done    bool
}

func (kv *KV) Begin() *KVTX {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	snap := kv.pg.snapshot()
	kv.pg.pin()
	tx := &KVTX{kv: kv, tree: NewBTree(snap), version: kv.version, pending: map[string]pend{}}
	kv.active[tx] = struct{}{}
	return tx
}

// release must be called with kv.mu held.
func (kv *KV) release(tx *KVTX) {
	delete(kv.active, tx)
	kv.pg.unpin()
	minV := kv.version
	for a := range kv.active {
		if a.version < minV {
			minV = a.version
		}
	}
	i := 0
	for i < len(kv.history) && kv.history[i].version <= minV {
		i++
	}
	kv.history = kv.history[i:]
}

func (tx *KVTX) Abort() {
	tx.kv.mu.Lock()
	defer tx.kv.mu.Unlock()
	if !tx.done {
		tx.done = true
		tx.kv.release(tx)
	}
}

func (tx *KVTX) Get(key string) (string, bool) {
	tx.reads = append(tx.reads, keyRange{lo: key, hi: key})
	if p, ok := tx.pending[key]; ok {
		return p.val, !p.del
	}
	return tx.tree.Get(key)
}

func (tx *KVTX) put(key string, p pend) {
	if len(key) == 0 || len(key) > maxKeySize {
		fail("key size out of range")
	}
	if len(p.val) > maxValSize {
		fail("value too large (max %d bytes)", maxValSize)
	}
	// a write depends on the previous state of the key -> counts as a read
	tx.reads = append(tx.reads, keyRange{lo: key, hi: key})
	if _, ok := tx.pending[key]; !ok {
		i := sort.SearchStrings(tx.keys, key)
		tx.keys = append(tx.keys, "")
		copy(tx.keys[i+1:], tx.keys[i:])
		tx.keys[i] = key
	}
	tx.pending[key] = p
}

func (tx *KVTX) Set(key, val string) { tx.put(key, pend{val: val}) }
func (tx *KVTX) Del(key string)      { tx.put(key, pend{del: true}) }

// Update applies an INSERT/UPDATE/UPSERT mode inside the transaction.
func (tx *KVTX) Update(req *UpdateReq) {
	old, ok := tx.Get(req.Key)
	if applyMode(req, old, ok) {
		tx.Set(req.Key, req.Val)
	}
}

// Scan visits keys in [lo, hi] (hiInf: no upper bound), ascending or
// descending, merging the snapshot with this transaction's pending writes.
// fn returns false to stop.
func (tx *KVTX) Scan(lo, hi string, hiInf, desc bool, fn func(k, v string) bool) {
	tx.reads = append(tx.reads, keyRange{lo: lo, hi: hi, hiInf: hiInf})
	in := func(k string) bool { return k >= lo && (hiInf || k <= hi) }
	var bc *Cursor
	var pi int
	if !desc {
		bc = tx.tree.Seek(lo)
		pi = sort.SearchStrings(tx.keys, lo)
	} else if hiInf {
		bc = tx.tree.Last()
		pi = len(tx.keys) - 1
	} else {
		bc = tx.tree.SeekLE(hi)
		pi = sort.Search(len(tx.keys), func(i int) bool { return tx.keys[i] > hi }) - 1
	}
	for {
		bv := bc.Valid() && in(bc.Key())
		pv := pi >= 0 && pi < len(tx.keys) && in(tx.keys[pi])
		if !bv && !pv {
			return
		}
		useB, useP := bv, pv
		if bv && pv {
			bk, pk := bc.Key(), tx.keys[pi]
			switch {
			case bk == pk: // pending overrides snapshot; consume both
			case (bk < pk) != desc:
				useP = false
			default:
				useB = false
			}
		}
		var k, v string
		emit := true
		if useP {
			k = tx.keys[pi]
			p := tx.pending[k]
			v, emit = p.val, !p.del
		} else {
			k, v = bc.Key(), bc.Val()
		}
		if useB {
			if desc {
				bc.Prev()
			} else {
				bc.Next()
			}
		}
		if useP {
			if desc {
				pi--
			} else {
				pi++
			}
		}
		if emit && !fn(k, v) {
			return
		}
	}
}

func (kv *KV) conflicts(tx *KVTX) bool {
	for _, h := range kv.history {
		if h.version <= tx.version {
			continue
		}
		for _, w := range h.writes {
			for _, r := range tx.reads {
				if r.has(w) {
					return true
				}
			}
		}
	}
	return false
}

// Commit validates (OCC) and durably applies the transaction.
func (tx *KVTX) Commit() (err error) {
	kv := tx.kv
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if tx.done {
		return errors.New("transaction already finished")
	}
	tx.done = true
	defer kv.release(tx)
	if kv.conflicts(tx) {
		return ErrConflict
	}
	if len(tx.keys) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			kv.pg.Rollback()
		}
	}()
	defer catchDBError(&err)
	t := NewBTree(kv.pg) // kv.pg.Root == last committed root
	for _, k := range tx.keys {
		if p := tx.pending[k]; p.del {
			t.Del(k)
		} else {
			t.Set(k, p.val)
		}
	}
	kv.pg.Commit()
	kv.version++
	kv.history = append(kv.history, committedTX{version: kv.version, writes: tx.keys})
	return nil
}
