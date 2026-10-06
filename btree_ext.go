package main

func (c *Cursor) descendRight(ptr uint64) {
	for {
		n := c.tree.load(ptr)
		if n.leaf {
			c.path = append(c.path, frame{n: n, idx: len(n.keys) - 1})
			return
		}
		idx := len(n.kids) - 1
		c.path = append(c.path, frame{n: n, idx: idx})
		ptr = n.kids[idx]
	}
}

// Prev moves the cursor one entry backwards (invalid when it falls off the front).
func (c *Cursor) Prev() {
	for len(c.path) > 0 {
		f := &c.path[len(c.path)-1]
		if f.idx > 0 {
			f.idx--
			if !f.n.leaf {
				c.descendRight(f.n.kids[f.idx])
			}
			return
		}
		c.path = c.path[:len(c.path)-1]
	}
}

// Last positions a cursor on the greatest key (invalid if the tree is empty).
func (t *BTree) Last() *Cursor {
	c := &Cursor{tree: t}
	if t.pg.Root != 0 {
		c.descendRight(t.pg.Root)
	}
	return c
}

// SeekLE positions a cursor on the greatest key <= key.
func (t *BTree) SeekLE(key string) *Cursor {
	c := t.Seek(key)
	if !c.Valid() {
		return t.Last()
	}
	if c.Key() != key {
		c.Prev()
	}
	return c
}

// update modes (book ch.8)
const (
	ModeUpsert     = 0
	ModeUpdateOnly = 1
	ModeInsertOnly = 2
)

type UpdateReq struct {
	Key, Val string
	Mode     int
	// out
	Added   bool   // a new key was added
	Updated bool   // a key was added or its value changed
	Old     string // value before the update
}

func applyMode(req *UpdateReq, old string, exists bool) bool {
	req.Old = old
	if (req.Mode == ModeInsertOnly && exists) || (req.Mode == ModeUpdateOnly && !exists) {
		return false
	}
	if exists && old == req.Val {
		return false
	}
	req.Added, req.Updated = !exists, true
	return true
}

func (t *BTree) Update(req *UpdateReq) {
	old, ok := t.Get(req.Key)
	if applyMode(req, old, ok) {
		t.Set(req.Key, req.Val)
	}
}
