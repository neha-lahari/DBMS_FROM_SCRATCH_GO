package main

type Column struct {
	Name    string
	Type    ColType
	NotNull bool
	Default *Expr
}

type IndexDef struct {
	Name   string
	ID     uint32
	Unique bool
	Cols   []int
}

type TableDef struct {
	Name    string
	ID      uint32
	Cols    []Column
	PK      []int // column positions forming the primary key
	Indexes []IndexDef
}

func (t *TableDef) ColIndex(name string) int {
	for i, c := range t.Cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

type ViewDef struct {
	Name  string
	Query string // raw SELECT text, re-parsed on each use
}

type ProcDef struct {
	Name       string
	IsFunction bool
	Params     []Param
	ReturnType ColType
	Body       string // raw procedural body text (statements between BEGIN/END), re-parsed on each call
}

type TriggerDef struct {
	Name   string
	Table  string
	Timing string // BEFORE / AFTER
	Event  string // INSERT / UPDATE / DELETE
	Body   string
}

type UserDef struct {
	Name     string
	Password string
}

// GrantSet: privileges a user holds, per table ("*" = all tables).
type GrantSet struct {
	User  string
	Privs map[string]map[string]bool // table -> privilege -> true
}

func newGrantSet(user string) *GrantSet {
	return &GrantSet{User: user, Privs: map[string]map[string]bool{}}
}

func (g *GrantSet) grant(table string, privs []string) {
	m, ok := g.Privs[table]
	if !ok {
		m = map[string]bool{}
		g.Privs[table] = m
	}
	for _, p := range privs {
		m[p] = true
	}
}

func (g *GrantSet) revoke(table string, privs []string) {
	m, ok := g.Privs[table]
	if !ok {
		return
	}
	for _, p := range privs {
		delete(m, p)
	}
}

func (g *GrantSet) has(table, priv string) bool {
	for _, t := range []string{table, "*"} {
		if m, ok := g.Privs[t]; ok {
			if m["ALL"] || m[priv] {
				return true
			}
		}
	}
	return false
}

type Catalog struct {
	Tables    map[string]*TableDef
	Views     map[string]*ViewDef
	Procs     map[string]*ProcDef // procedures and functions share a namespace, like most SQL dialects
	Triggers  map[string]*TriggerDef
	TrigOrder []string // creation order, for deterministic firing
	Users     map[string]*UserDef
	Grants    map[string]*GrantSet
	nextID    uint32
}

func NewCatalog() *Catalog {
	c := &Catalog{
		Tables:   map[string]*TableDef{},
		Views:    map[string]*ViewDef{},
		Procs:    map[string]*ProcDef{},
		Triggers: map[string]*TriggerDef{},
		Users:    map[string]*UserDef{},
		Grants:   map[string]*GrantSet{},
		nextID:   100,
	}
	c.Users["admin"] = &UserDef{Name: "admin"}
	return c
}

func (c *Catalog) clone() *Catalog {
	n := &Catalog{
		Tables: map[string]*TableDef{}, Views: map[string]*ViewDef{},
		Procs: map[string]*ProcDef{}, Triggers: map[string]*TriggerDef{},
		Users: map[string]*UserDef{}, Grants: map[string]*GrantSet{}, nextID: c.nextID,
	}
	for k, v := range c.Tables {
		cp := *v
		n.Tables[k] = &cp
	}
	for k, v := range c.Views {
		cp := *v
		n.Views[k] = &cp
	}
	for k, v := range c.Procs {
		cp := *v
		n.Procs[k] = &cp
	}
	for k, v := range c.Triggers {
		cp := *v
		n.Triggers[k] = &cp
	}
	n.TrigOrder = append([]string(nil), c.TrigOrder...)
	for k, v := range c.Users {
		cp := *v
		n.Users[k] = &cp
	}
	for k, v := range c.Grants {
		cp := newGrantSet(k)
		for t, m := range v.Privs {
			cp.Privs[t] = map[string]bool{}
			for p := range m {
				cp.Privs[t][p] = true
			}
		}
		n.Grants[k] = cp
	}
	return n
}

func nsKey(ns uint32, rest string) []byte {
	var k []byte
	putU32(&k, ns)
	k = append(k, rest...)
	return k
}

func (c *Catalog) AllocID(t *BTree) uint32 {
	id := c.nextID
	c.nextID++
	var v []byte
	putU32(&v, c.nextID)
	t.Set(string(nsKey(0, "next_id")), string(v))
	return id
}

// ---- generic (de)serialisation helpers shared by all catalog records
func putStr(s *[]byte, v string) {
	putU16(s, uint16(len(v)))
	*s = append(*s, v...)
}
func getStr(r *reader) string {
	n := r.u16()
	return r.str(int(n))
}

func serializeTable(t *TableDef) []byte {
	var s []byte
	putStr(&s, t.Name)
	putU32(&s, t.ID)
	putU16(&s, uint16(len(t.Cols)))
	for _, c := range t.Cols {
		putStr(&s, c.Name)
		putU8(&s, byte(c.Type))
		if c.NotNull {
			putU8(&s, 1)
		} else {
			putU8(&s, 0)
		}
	}
	putU16(&s, uint16(len(t.PK)))
	for _, p := range t.PK {
		putU16(&s, uint16(p))
	}
	putU16(&s, uint16(len(t.Indexes)))
	for _, ix := range t.Indexes {
		putStr(&s, ix.Name)
		putU32(&s, ix.ID)
		if ix.Unique {
			putU8(&s, 1)
		} else {
			putU8(&s, 0)
		}
		putU16(&s, uint16(len(ix.Cols)))
		for _, c := range ix.Cols {
			putU16(&s, uint16(c))
		}
	}
	return s
}

func deserializeTable(data string) *TableDef {
	r := newReader([]byte(data))
	t := &TableDef{}
	t.Name = getStr(r)
	t.ID = r.u32()
	nc := r.u16()
	for i := uint16(0); i < nc; i++ {
		c := Column{}
		c.Name = getStr(r)
		c.Type = ColType(r.u8())
		c.NotNull = r.u8() != 0
		t.Cols = append(t.Cols, c)
	}
	np := r.u16()
	for i := uint16(0); i < np; i++ {
		t.PK = append(t.PK, int(r.u16()))
	}
	ni := r.u16()
	for i := uint16(0); i < ni; i++ {
		ix := IndexDef{}
		ix.Name = getStr(r)
		ix.ID = r.u32()
		ix.Unique = r.u8() != 0
		k := r.u16()
		for j := uint16(0); j < k; j++ {
			ix.Cols = append(ix.Cols, int(r.u16()))
		}
		t.Indexes = append(t.Indexes, ix)
	}
	return t
}

func (c *Catalog) PutTable(t *BTree, def *TableDef) {
	t.Set(string(nsKey(1, def.Name)), string(serializeTable(def)))
	c.Tables[def.Name] = def
}
func (c *Catalog) RemoveTable(t *BTree, name string) {
	t.Del(string(nsKey(1, name)))
	delete(c.Tables, name)
}

func (c *Catalog) PutView(t *BTree, v *ViewDef) {
	var s []byte
	putStr(&s, v.Name)
	putStr(&s, v.Query)
	t.Set(string(nsKey(2, v.Name)), string(s))
	c.Views[v.Name] = v
}
func (c *Catalog) RemoveView(t *BTree, name string) {
	t.Del(string(nsKey(2, name)))
	delete(c.Views, name)
}

func (c *Catalog) PutProc(t *BTree, pd *ProcDef) {
	var s []byte
	putStr(&s, pd.Name)
	if pd.IsFunction {
		putU8(&s, 1)
	} else {
		putU8(&s, 0)
	}
	putU16(&s, uint16(len(pd.Params)))
	for _, pr := range pd.Params {
		putStr(&s, pr.Name)
		putU8(&s, byte(pr.Type))
	}
	putU8(&s, byte(pd.ReturnType))
	putStr(&s, pd.Body)
	t.Set(string(nsKey(3, pd.Name)), string(s))
	c.Procs[pd.Name] = pd
}
func (c *Catalog) RemoveProc(t *BTree, name string) {
	t.Del(string(nsKey(3, name)))
	delete(c.Procs, name)
}

func (c *Catalog) PutTrigger(t *BTree, td *TriggerDef) {
	var s []byte
	putStr(&s, td.Name)
	putStr(&s, td.Table)
	putStr(&s, td.Timing)
	putStr(&s, td.Event)
	putStr(&s, td.Body)
	t.Set(string(nsKey(4, td.Name)), string(s))
	if _, exists := c.Triggers[td.Name]; !exists {
		c.TrigOrder = append(c.TrigOrder, td.Name)
	}
	c.Triggers[td.Name] = td
}
func (c *Catalog) RemoveTrigger(t *BTree, name string) {
	t.Del(string(nsKey(4, name)))
	delete(c.Triggers, name)
	for i, n := range c.TrigOrder {
		if n == name {
			c.TrigOrder = append(c.TrigOrder[:i], c.TrigOrder[i+1:]...)
			break
		}
	}
}

func (c *Catalog) PutUser(t *BTree, u *UserDef) {
	var s []byte
	putStr(&s, u.Name)
	putStr(&s, u.Password)
	t.Set(string(nsKey(5, u.Name)), string(s))
	c.Users[u.Name] = u
}
func (c *Catalog) RemoveUser(t *BTree, name string) {
	t.Del(string(nsKey(5, name)))
	delete(c.Users, name)
	t.Del(string(nsKey(6, name)))
	delete(c.Grants, name)
}

func (c *Catalog) PutGrants(t *BTree, g *GrantSet) {
	var s []byte
	putU16(&s, uint16(len(g.Privs)))
	for table, m := range g.Privs {
		putStr(&s, table)
		putU16(&s, uint16(len(m)))
		for priv := range m {
			putStr(&s, priv)
		}
	}
	t.Set(string(nsKey(6, g.User)), string(s))
	c.Grants[g.User] = g
}

func (c *Catalog) GrantsFor(user string) *GrantSet {
	if g, ok := c.Grants[user]; ok {
		return g
	}
	return newGrantSet(user)
}

// Load (re)reads the whole catalog from storage.
func (c *Catalog) Load(t *BTree) {
	c.Tables = map[string]*TableDef{}
	c.Views = map[string]*ViewDef{}
	c.Procs = map[string]*ProcDef{}
	c.Triggers = map[string]*TriggerDef{}
	c.TrigOrder = nil
	c.Users = map[string]*UserDef{}
	c.Grants = map[string]*GrantSet{}
	c.nextID = 100

	if v, ok := t.Get(string(nsKey(0, "next_id"))); ok && len(v) == 4 {
		c.nextID = uint32(binary64([]byte(v)) & 0xFFFFFFFF)
	}
	scanNS := func(ns uint32, fn func(key, val string)) {
		lo := string(nsKey(ns, ""))
		hi := string(PrefixSuccessor([]byte(lo)))
		for cur := t.Seek(lo); cur.Valid() && cur.Key() < hi; cur.Next() {
			fn(cur.Key(), cur.Val())
		}
	}
	scanNS(1, func(_, v string) {
		d := deserializeTable(v)
		c.Tables[d.Name] = d
	})
	scanNS(2, func(_, v string) {
		r := newReader([]byte(v))
		vd := &ViewDef{Name: getStr(r), Query: getStr(r)}
		c.Views[vd.Name] = vd
	})
	scanNS(3, func(_, v string) {
		r := newReader([]byte(v))
		pd := &ProcDef{Name: getStr(r)}
		pd.IsFunction = r.u8() != 0
		np := r.u16()
		for i := uint16(0); i < np; i++ {
			pr := Param{Name: getStr(r)}
			pr.Type = ColType(r.u8())
			pd.Params = append(pd.Params, pr)
		}
		pd.ReturnType = ColType(r.u8())
		pd.Body = getStr(r)
		c.Procs[pd.Name] = pd
	})
	scanNS(4, func(_, v string) {
		r := newReader([]byte(v))
		td := &TriggerDef{Name: getStr(r), Table: getStr(r), Timing: getStr(r), Event: getStr(r), Body: getStr(r)}
		c.Triggers[td.Name] = td
		c.TrigOrder = append(c.TrigOrder, td.Name)
	})
	scanNS(5, func(_, v string) {
		r := newReader([]byte(v))
		u := &UserDef{Name: getStr(r), Password: getStr(r)}
		c.Users[u.Name] = u
	})
	if _, ok := c.Users["admin"]; !ok {
		c.Users["admin"] = &UserDef{Name: "admin"}
	}
	scanNS(6, func(key, v string) {
		user := key[4:]
		r := newReader([]byte(v))
		g := newGrantSet(user)
		n := r.u16()
		for i := uint16(0); i < n; i++ {
			table := getStr(r)
			np := r.u16()
			m := map[string]bool{}
			for j := uint16(0); j < np; j++ {
				m[getStr(r)] = true
			}
			g.Privs[table] = m
		}
		c.Grants[user] = g
	})
}
