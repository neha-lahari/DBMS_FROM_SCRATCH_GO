package main

type node struct {
	leaf bool
	keys []string
	vals []string // leaves only
	kids []uint64 // internal nodes only
}

func (n *node) entrySize(i int) int {
	vl := 0
	if n.leaf {
		vl = len(n.vals[i])
	}
	return 8 + 2 + 4 + len(n.keys[i]) + vl
}

func (n *node) size() int {
	s := 4
	for i := range n.keys {
		s += n.entrySize(i)
	}
	return s
}

func (n *node) encode() []byte {
	var s []byte
	if n.leaf {
		putU16(&s, 2)
	} else {
		putU16(&s, 1)
	}
	putU16(&s, uint16(len(n.keys)))
	for i := range n.keys {
		if n.leaf {
			putU64(&s, 0)
		} else {
			putU64(&s, n.kids[i])
		}
	}
	off := 0
	for i := range n.keys {
		off += n.entrySize(i) - 10
		putU16(&s, uint16(off))
	}
	for i, k := range n.keys {
		v := ""
		if n.leaf {
			v = n.vals[i]
		}
		putU16(&s, uint16(len(k)))
		putU16(&s, uint16(len(v)))
		s = append(s, k...)
		s = append(s, v...)
	}
	if len(s) > pageSize {
		fail("internal error: node too large")
	}
	return s
}

func decodeNode(pg *page) *node {
	r := newReader(pg[:])
	typ := r.u16()
	cnt := int(r.u16())
	n := &node{}
	switch typ {
	case 2:
		n.leaf = true
	case 1:
		n.leaf = false
	default:
		fail("corrupt page: bad node type")
	}
	ptrs := make([]uint64, cnt)
	for i := range ptrs {
		ptrs[i] = r.u64()
	}
	offs := make([]int, cnt)
	for i := range offs {
		offs[i] = int(r.u16())
	}
	base := r.pos
	for i := 0; i < cnt; i++ {
		kl := int(r.u16())
		vl := int(r.u16())
		n.keys = append(n.keys, r.str(kl))
		v := r.str(vl)
		if r.pos-base != offs[i] {
			fail("corrupt page: bad offset array")
		}
		if n.leaf {
			n.vals = append(n.vals, v)
		} else {
			n.kids = append(n.kids, ptrs[i])
		}
	}
	return n
}

type BTree struct{ pg *Pager }

func NewBTree(pg *Pager) *BTree { return &BTree{pg: pg} }

func (t *BTree) load(ptr uint64) *node { return decodeNode(t.pg.Read(ptr)) }
func (t *BTree) store(n *node) uint64  { return t.pg.Alloc(makePage(n.encode())) }

// route finds which child of internal node n may contain key.
func route(n *node, key string) int {
	lo, hi := 1, len(n.keys)
	for lo < hi {
		mid := (lo + hi) / 2
		if n.keys[mid] <= key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

func lowerBound(keys []string, key string) int {
	lo, hi := 0, len(keys)
	for lo < hi {
		mid := (lo + hi) / 2
		if keys[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// Get returns (value, true) if key is present.
func (t *BTree) Get(key string) (string, bool) {
	ptr := t.pg.Root
	for ptr != 0 {
		n := t.load(ptr)
		if n.leaf {
			i := lowerBound(n.keys, key)
			if i < len(n.keys) && n.keys[i] == key {
				return n.vals[i], true
			}
			return "", false
		}
		ptr = n.kids[route(n, key)]
	}
	return "", false
}

func (t *BTree) Has(key string) bool { _, ok := t.Get(key); return ok }

// ---------------------------------------------------------------- insert
type piece struct {
	key string
	ptr uint64
}

// splitNode breaks an oversized node into pieces that each fit in one page.
func splitNode(n *node) []*node {
	if n.size() <= pageSize {
		return []*node{n}
	}
	cnt := len(n.keys)
	slice := func(a, b int) *node {
		r := &node{leaf: n.leaf}
		r.keys = append(r.keys, n.keys[a:b]...)
		if n.leaf {
			r.vals = append(r.vals, n.vals[a:b]...)
		} else {
			r.kids = append(r.kids, n.kids[a:b]...)
		}
		return r
	}
	total := n.size()
	acc, cut := 4, 0
	for i := 0; i < cnt; i++ {
		acc += n.entrySize(i)
		if acc >= total/2 {
			cut = i + 1
			break
		}
	}
	if cut >= 1 && cut < cnt {
		l, r := slice(0, cut), slice(cut, cnt)
		if l.size() <= pageSize && r.size() <= pageSize {
			return []*node{l, r}
		}
	}
	// Fallback (huge entries): greedy fill.
	var out []*node
	start, cur := 0, 4
	for i := 0; i < cnt; i++ {
		es := n.entrySize(i)
		if cur+es > pageSize && i > start {
			out = append(out, slice(start, i))
			start = i
			cur = 4
		}
		cur += es
	}
	out = append(out, slice(start, cnt))
	return out
}

func (t *BTree) storeSplit(n *node) []piece {
	var out []piece
	for _, part := range splitNode(n) {
		out = append(out, piece{key: part.keys[0], ptr: t.store(part)})
	}
	return out
}

func (t *BTree) insertRec(ptr uint64, key, val string) []piece {
	n := t.load(ptr)
	t.pg.Free(ptr) // copy-on-write: old version is garbage after commit
	if n.leaf {
		i := lowerBound(n.keys, key)
		if i < len(n.keys) && n.keys[i] == key {
			n.vals[i] = val
		} else {
			n.keys = append(n.keys, "")
			copy(n.keys[i+1:], n.keys[i:])
			n.keys[i] = key
			n.vals = append(n.vals, "")
			copy(n.vals[i+1:], n.vals[i:])
			n.vals[i] = val
		}
	} else {
		i := route(n, key)
		pieces := t.insertRec(n.kids[i], key, val)
		oldKey := n.keys[i]
		n.keys = append(n.keys[:i], n.keys[i+1:]...)
		n.kids = append(n.kids[:i], n.kids[i+1:]...)
		for j, pc := range pieces {
			k := pc.key
			if j == 0 {
				k = oldKey
			}
			idx := i + j
			n.keys = append(n.keys, "")
			copy(n.keys[idx+1:], n.keys[idx:])
			n.keys[idx] = k
			n.kids = append(n.kids, 0)
			copy(n.kids[idx+1:], n.kids[idx:])
			n.kids[idx] = pc.ptr
		}
	}
	return t.storeSplit(n)
}

// Set inserts or overwrites key -> val.
func (t *BTree) Set(key, val string) {
	if len(key) == 0 || len(key) > maxKeySize {
		fail("key size out of range")
	}
	if len(val) > maxValSize {
		fail("value too large (max %d bytes)", maxValSize)
	}
	if t.pg.Root == 0 {
		n := &node{leaf: true, keys: []string{key}, vals: []string{val}}
		t.pg.Root = t.store(n)
		return
	}
	pieces := t.insertRec(t.pg.Root, key, val)
	for len(pieces) > 1 { // root split: grow the tree by one level
		parent := &node{leaf: false}
		for _, pc := range pieces {
			parent.keys = append(parent.keys, pc.key)
			parent.kids = append(parent.kids, pc.ptr)
		}
		pieces = t.storeSplit(parent)
	}
	t.pg.Root = pieces[0].ptr
}

// ---------------------------------------------------------------- delete
type delResult struct {
	found bool
	ptr   uint64 // 0 => node became empty and disappeared
}

func (t *BTree) deleteRec(ptr uint64, key string) delResult {
	n := t.load(ptr)
	if n.leaf {
		i := lowerBound(n.keys, key)
		if i >= len(n.keys) || n.keys[i] != key {
			return delResult{ptr: ptr}
		}
		t.pg.Free(ptr)
		n.keys = append(n.keys[:i], n.keys[i+1:]...)
		n.vals = append(n.vals[:i], n.vals[i+1:]...)
		if len(n.keys) == 0 {
			return delResult{found: true, ptr: 0}
		}
		return delResult{found: true, ptr: t.store(n)}
	}
	i := route(n, key)
	child := t.deleteRec(n.kids[i], key)
	if !child.found {
		return delResult{ptr: ptr}
	}
	t.pg.Free(ptr)
	if child.ptr == 0 {
		n.keys = append(n.keys[:i], n.keys[i+1:]...)
		n.kids = append(n.kids[:i], n.kids[i+1:]...)
	} else {
		n.kids[i] = child.ptr
		// Merge an underfull child with a neighbour when the result fits in one page.
		c := t.load(child.ptr)
		if c.size() < pageSize/4 && len(n.kids) > 1 {
			sib := i + 1
			if sib >= len(n.kids) {
				sib = i - 1
			}
			s := t.load(n.kids[sib])
			if c.size()+s.size()-4 <= pageSize {
				left, right := i, sib
				if sib < i {
					left, right = sib, i
				}
				L, R := c, s
				if left == i {
					// c is left
				} else {
					L, R = s, c
				}
				L.keys = append(L.keys, R.keys...)
				L.vals = append(L.vals, R.vals...)
				L.kids = append(L.kids, R.kids...)
				t.pg.Free(n.kids[left])
				t.pg.Free(n.kids[right])
				n.kids[left] = t.store(L)
				n.keys = append(n.keys[:right], n.keys[right+1:]...)
				n.kids = append(n.kids[:right], n.kids[right+1:]...)
			}
		}
	}
	if len(n.keys) == 0 {
		return delResult{found: true, ptr: 0}
	}
	return delResult{found: true, ptr: t.store(n)}
}

// Del removes key, returning false if it was absent.
func (t *BTree) Del(key string) bool {
	if t.pg.Root == 0 {
		return false
	}
	r := t.deleteRec(t.pg.Root, key)
	if !r.found {
		return false
	}
	t.pg.Root = r.ptr
	// Shrink the tree while the root is an internal node with a single child.
	for t.pg.Root != 0 {
		n := t.load(t.pg.Root)
		if n.leaf || len(n.kids) != 1 {
			break
		}
		only := n.kids[0]
		t.pg.Free(t.pg.Root)
		t.pg.Root = only
	}
	return true
}

// ---------------------------------------------------------------- cursor
type frame struct {
	n   *node
	idx int
}

// Cursor walks the tree in key order starting from a Seek() call.
type Cursor struct {
	tree *BTree
	path []frame
}

func (c *Cursor) Valid() bool { return len(c.path) > 0 }
func (c *Cursor) Key() string { f := c.path[len(c.path)-1]; return f.n.keys[f.idx] }
func (c *Cursor) Val() string { f := c.path[len(c.path)-1]; return f.n.vals[f.idx] }

func (c *Cursor) descendLeft(ptr uint64) {
	for {
		n := c.tree.load(ptr)
		var child uint64
		if !n.leaf {
			child = n.kids[0]
		}
		c.path = append(c.path, frame{n: n, idx: 0})
		if n.leaf {
			return
		}
		ptr = child
	}
}

// settle ensures the top frame is a leaf positioned on a valid entry, or
// empties the path (end of iteration).
func (c *Cursor) settle() {
	for len(c.path) > 0 {
		f := &c.path[len(c.path)-1]
		if f.n.leaf {
			if f.idx < len(f.n.keys) {
				return
			}
			c.path = c.path[:len(c.path)-1]
			if len(c.path) == 0 {
				return
			}
			c.path[len(c.path)-1].idx++
		} else {
			if f.idx < len(f.n.kids) {
				c.descendLeft(f.n.kids[f.idx])
			} else {
				c.path = c.path[:len(c.path)-1]
				if len(c.path) == 0 {
					return
				}
				c.path[len(c.path)-1].idx++
			}
		}
	}
}

func (c *Cursor) Next() {
	c.path[len(c.path)-1].idx++
	c.settle()
}

// Seek positions a cursor at the first entry with key >= `key`.
func (t *BTree) Seek(key string) *Cursor {
	c := &Cursor{tree: t}
	ptr := t.pg.Root
	for ptr != 0 {
		n := t.load(ptr)
		if n.leaf {
			i := lowerBound(n.keys, key)
			c.path = append(c.path, frame{n: n, idx: i})
			break
		}
		i := route(n, key)
		ptr = n.kids[i]
		c.path = append(c.path, frame{n: n, idx: i})
	}
	c.settle()
	return c
}

// ---------------------------------------------------------------- verify
func (t *BTree) verifyRec(ptr uint64, lo, hi *string) int {
	n := t.load(ptr)
	if n.size() > pageSize {
		fail("verify: oversized node")
	}
	if len(n.keys) == 0 {
		fail("verify: empty node")
	}
	if n.leaf {
		for i := range n.keys {
			if i > 0 && !(n.keys[i-1] < n.keys[i]) {
				fail("verify: leaf keys not sorted")
			}
			if lo != nil && n.keys[i] < *lo {
				fail("verify: key below range")
			}
			if hi != nil && !(n.keys[i] < *hi) {
				fail("verify: key above range")
			}
		}
		return len(n.keys)
	}
	total := 0
	for i := range n.kids {
		if i > 1 && !(n.keys[i-1] < n.keys[i]) {
			fail("verify: separators not sorted")
		}
		l, h := lo, hi
		if i != 0 {
			l = &n.keys[i]
		}
		if i+1 < len(n.kids) {
			h = &n.keys[i+1]
		}
		total += t.verifyRec(n.kids[i], l, h)
	}
	return total
}

// Verify walks the whole tree and checks every structural invariant,
// returning the number of entries found.
func (t *BTree) Verify() int {
	if t.pg.Root == 0 {
		return 0
	}
	return t.verifyRec(t.pg.Root, nil, nil)
}
