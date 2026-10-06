package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"sync"
)

var fileMagic = [8]byte{'M', 'Y', 'D', 'B', 'F', 'I', 'L', 'E'}

type page [pageSize]byte

func makePage(data []byte) *page {
	if len(data) > pageSize {
		fail("internal error: page overflow")
	}
	p := &page{}
	copy(p[:], data)
	return p
}

type meta struct {
	seq, root, npages, flHead uint64
}

// savepoint captures everything needed to undo an in-progress (uncommitted)
// transaction, or to resume one across nested statement-level save/restore.
type savepoint struct {
	dirty   map[uint64]*page
	fresh   map[uint64]bool
	avail   []uint64
	pending []uint64
	npages  uint64
	root    uint64
}

type Pager struct {
	Root uint64 // root page of the B+tree (0 = empty)

	f  *os.File
	mu sync.Mutex // guards the maps below; reads can race with each other but not with an allocation

	seq, metaRoot, npages uint64
	avail                 []uint64 // reusable page ids
	pending               []uint64 // freed in this txn, reusable after commit
	oldChain              []uint64 // pages holding the currently-persisted free list
	dirty                 map[uint64]*page
	cache                 map[uint64]*page
	fresh                 map[uint64]bool // allocated (not yet committed) this txn
	committed             savepoint
	pinned                int      // open snapshot readers (KV layer)
	held                  []uint64 // freed pages a snapshot reader may still need
}

const flCap = (pageSize - 16) / 8

func fnv1a(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

func OpenPager(path string) (*Pager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, dbErrorf("cannot open database file '%s': %v", path, err)
	}
	if err := lockDatabaseFile(f); err != nil {
		f.Close()
		return nil, dbErrorf("database file is locked by another process")
	}
	p := &Pager{
		f:     f,
		dirty: map[uint64]*page{},
		cache: map[uint64]*page{},
		fresh: map[uint64]bool{},
	}
	if err := p.init(); err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

func (p *Pager) init() (err error) {
	defer catchDBError(&err)
	fi, e := p.f.Stat()
	if e != nil {
		fail("stat failed: %v", e)
	}
	if fi.Size() == 0 { // brand new file: two valid metas, seq 0 and 1
		p.writeMeta(0, meta{npages: 2})
		p.writeMeta(1, meta{seq: 1, npages: 2})
		p.sync()
	}
	a, okA := p.readMeta(0)
	b, okB := p.readMeta(1)
	if !okA && !okB {
		fail("not a valid database file (or both meta pages corrupt)")
	}
	m := b
	if okA && (!okB || a.seq > b.seq) {
		m = a
	}
	p.seq = m.seq
	p.Root = m.root
	p.metaRoot = m.root
	p.npages = m.npages
	p.loadFreeList(m.flHead)
	p.committed = p.save()
	return nil
}

func (p *Pager) Close() error { return p.f.Close() }

// ------------------------------------------------------------- raw I/O
func (p *Pager) readAt(off int64, buf []byte) int {
	n, err := p.f.ReadAt(buf, off)
	if err != nil && n == 0 {
		return 0
	}
	return n
}

func (p *Pager) writeAt(off int64, buf []byte) {
	if _, err := p.f.WriteAt(buf, off); err != nil {
		fail("write failed: %v", err)
	}
}

func (p *Pager) sync() {
	// fsync: flush OS page cache + device cache. This is what makes commits durable.
	if err := p.f.Sync(); err != nil {
		fail("fsync failed: %v", err)
	}
}

// ------------------------------------------------------------------ meta
func (p *Pager) readMeta(slot int) (meta, bool) {
	buf := make([]byte, pageSize)
	if p.readAt(int64(slot)*pageSize, buf) < 48 {
		return meta{}, false
	}
	if string(buf[:8]) != string(fileMagic[:]) {
		return meta{}, false
	}
	if binary64(buf[40:48]) != fnv1a(buf[:40]) { // torn / corrupt write
		return meta{}, false
	}
	return meta{
		seq:    binary64(buf[8:16]),
		root:   binary64(buf[16:24]),
		npages: binary64(buf[24:32]),
		flHead: binary64(buf[32:40]),
	}, true
}

func binary64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func (p *Pager) writeMeta(slot int, m meta) {
	var s []byte
	s = append(s, fileMagic[:]...)
	putU64(&s, m.seq)
	putU64(&s, m.root)
	putU64(&s, m.npages)
	putU64(&s, m.flHead)
	putU64(&s, fnv1a(s))
	pg := makePage(s)
	p.writeAt(int64(slot)*pageSize, pg[:])
}

func (p *Pager) loadFreeList(head uint64) {
	cur := head
	guard := 0
	for cur != 0 {
		guard++
		if uint64(guard) > p.npages || cur >= p.npages {
			fail("corrupt free list")
		}
		p.oldChain = append(p.oldChain, cur)
		pg := p.readDisk(cur)
		next := binary64(pg[0:8])
		count := uint32(binary64(pg[8:12]))
		if count > flCap {
			fail("corrupt free list page")
		}
		for i := uint32(0); i < count; i++ {
			off := 16 + 8*i
			p.avail = append(p.avail, binary64(pg[off:off+8]))
		}
		cur = next
	}
}

// ----------------------------------------------------------- page access
func (p *Pager) readDisk(id uint64) *page {
	pg := &page{}
	if n := p.readAt(int64(id)*pageSize, pg[:]); n != pageSize {
		fail("short read: database file truncated?")
	}
	return pg
}

func (p *Pager) Read(id uint64) *page {
	if id < 2 || id >= p.npages {
		fail("corrupt database: bad page pointer")
	}
	if d, ok := p.dirty[id]; ok {
		return d
	}
	p.mu.Lock()
	if c, ok := p.cache[id]; ok {
		p.mu.Unlock()
		return c
	}
	p.mu.Unlock()
	pg := p.readDisk(id)
	p.mu.Lock()
	if len(p.cache) > 16384 {
		p.cache = map[uint64]*page{}
	}
	p.cache[id] = pg
	p.mu.Unlock()
	return pg
}

func (p *Pager) Alloc(data *page) uint64 {
	var id uint64
	if n := len(p.avail); n > 0 {
		id = p.avail[n-1]
		p.avail = p.avail[:n-1]
	} else {
		id = p.npages
		p.npages++
	}
	p.dirty[id] = data
	p.fresh[id] = true
	return id
}

func (p *Pager) Free(id uint64) {
	if p.fresh[id] { // never reached disk -> reuse straight away
		delete(p.fresh, id)
		delete(p.dirty, id)
		p.avail = append(p.avail, id)
	} else {
		p.pending = append(p.pending, id)
	}
}

// ------------------------------------------------------------ transactions
func (p *Pager) save() savepoint {
	sp := savepoint{
		dirty:   make(map[uint64]*page, len(p.dirty)),
		fresh:   make(map[uint64]bool, len(p.fresh)),
		avail:   append([]uint64(nil), p.avail...),
		pending: append([]uint64(nil), p.pending...),
		npages:  p.npages,
		root:    p.Root,
	}
	for k, v := range p.dirty {
		sp.dirty[k] = v
	}
	for k, v := range p.fresh {
		sp.fresh[k] = v
	}
	return sp
}

func (p *Pager) Restore(sp savepoint) {
	p.dirty = sp.dirty
	p.fresh = sp.fresh
	p.avail = sp.avail
	p.pending = sp.pending
	p.npages = sp.npages
	p.Root = sp.root
}

func (p *Pager) Rollback() { p.Restore(p.committed) }

func (p *Pager) PageCount() uint64     { return p.npages }
func (p *Pager) FreePageCount() uint64 { return uint64(len(p.avail)) }

func (p *Pager) Commit() {
	if len(p.dirty) == 0 && len(p.pending) == 0 && p.Root == p.metaRoot {
		return // nothing changed
	}
	defer func() {
		if r := recover(); r != nil {
			p.Rollback()
			panic(r)
		}
	}()

	// 1. Persist the free list: everything reusable next transaction.
	nav0 := len(p.avail)
	extras := append(append(append([]uint64(nil), p.pending...), p.held...), p.oldChain...)
	nPH := len(p.pending) + len(p.held)
	var chain []uint64
	for {
		total := len(p.avail) + len(extras)
		need := (total + flCap - 1) / flCap
		if len(chain) >= need {
			break
		}
		if n := len(p.avail); n > 0 {
			chain = append(chain, p.avail[n-1])
			p.avail = p.avail[:n-1]
		} else {
			chain = append(chain, p.npages)
			p.npages++
		}
	}
	_ = nav0
	ids := append(append([]uint64(nil), p.avail...), extras...)
	nav := len(p.avail)
	for i := range chain {
		var s []byte
		if i+1 < len(chain) {
			putU64(&s, chain[i+1])
		} else {
			putU64(&s, 0)
		}
		from := i * flCap
		to := from + flCap
		if to > len(ids) {
			to = len(ids)
		}
		putU32(&s, uint32(to-from))
		putU32(&s, 0)
		for j := from; j < to; j++ {
			putU64(&s, ids[j])
		}
		p.dirty[chain[i]] = makePage(s)
	}
	var flHead uint64
	if len(chain) > 0 {
		flHead = chain[0]
	}

	// 2. Write every new page, then make them durable.
	for id, pg := range p.dirty {
		p.writeAt(int64(id)*pageSize, pg[:])
	}
	p.sync()

	// 3. Flip the meta page (the commit point) and make *that* durable.
	m := meta{seq: p.seq + 1, root: p.Root, npages: p.npages, flHead: flHead}
	p.writeMeta(int(m.seq%2), m)
	p.sync()

	// 4. Update in-memory state.
	p.seq = m.seq
	p.metaRoot = p.Root
	p.mu.Lock()
	if len(p.cache) > 16384 {
		p.cache = map[uint64]*page{}
	}
	for id, pg := range p.dirty {
		p.cache[id] = pg
	}
	p.mu.Unlock()
	p.dirty = map[uint64]*page{}
	p.fresh = map[uint64]bool{}
	if p.pinned > 0 { // snapshot readers may still walk the old tree: hold its pages back
		p.held = append(append([]uint64(nil), p.pending...), p.held...)
		p.avail = append(append([]uint64(nil), ids[:nav]...), ids[nav+nPH:]...)
	} else {
		p.held = nil
		p.avail = ids
	}
	p.pending = nil
	p.oldChain = chain
	p.committed = p.save()
}

var _ = fmt.Sprintf // keep fmt imported even if unused by future edits

// snapshot returns a read-only Pager view of the last committed tree. It
// reads straight from the file (own empty caches), so later commits cannot
// disturb it as long as the pages it needs are not recycled (see pinned/held).
func (p *Pager) snapshot() *Pager {
	return &Pager{f: p.f, Root: p.metaRoot, metaRoot: p.metaRoot, npages: p.npages,
		dirty: map[uint64]*page{}, cache: map[uint64]*page{}, fresh: map[uint64]bool{}}
}

func (p *Pager) pin() { p.pinned++ }

func (p *Pager) unpin() {
	p.pinned--
	if p.pinned == 0 {
		p.avail = append(p.avail, p.held...)
		p.held = nil
	}
}
