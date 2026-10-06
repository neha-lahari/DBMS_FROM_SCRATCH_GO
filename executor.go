package main

import (
	"sort"
	"strings"
)

type Row = []Value

type StoredRow struct {
	Key  string
	Vals Row
}

type Result struct {
	IsQuery  bool
	Columns  []string
	Rows     []Row
	Message  string
	Affected int64
}

func msgResult(m string, affected int64) Result { return Result{Message: m, Affected: affected} }

// Executor runs statements for one session. CurUser/trigger depth support
// DCL privilege checks and trigger-recursion limits.
type Executor struct {
	tree         *BTree
	cat          *Catalog
	CurUser      string
	triggerDepth int
	callDepth    int
	procResult   Result

	// frameVars is the current procedure/function/trigger call frame's local
	// variables ("@x" -> Value), or nil when executing ordinary top-level
	// SQL. A map, so nested statement execution shares the same frame by
	// reference; proc.go saves/restores this across CALL and trigger firing.
	frameVars map[string]Value

	// trigTable/trigOld/trigNew are set while a trigger body is executing
	// (see trigger.go), so NEW.col / OLD.col expressions and `SET NEW.col =`
	// can be resolved without threading them through bindExpr.
	trigTable *TableDef
	trigOld   Row
	trigNew   Row

	// subqStack remembers the (schema, outer-schema-chain) of each query
	// currently being planned/evaluated, innermost last, so a subquery
	// expression encountered during row evaluation knows its own outer
	// scope chain. See select.go.
	subqStack []subqCtx
}

func NewExecutor(tree *BTree, cat *Catalog) *Executor {
	return &Executor{tree: tree, cat: cat, CurUser: "admin"}
}

// ctx/ctxAgg build an EvalCtx that carries this executor's current
// procedure-local variables, trigger NEW/OLD rows, and subquery/function
// hooks; every row-evaluation site should use these rather than
// constructing &EvalCtx{} directly, so @vars, NEW/OLD, subqueries and
// user-defined functions all work uniformly regardless of nesting.
func (ex *Executor) ctx(row Row, outer *EvalCtx) *EvalCtx {
	return &EvalCtx{Row: row, Outer: outer, Vars: ex.frameVars, SubEval: ex.subEval,
		CallFunc: ex.callUserFunc, TrigTable: ex.trigTable, TrigNew: ex.trigNew, TrigOld: ex.trigOld}
}
func (ex *Executor) ctxAgg(row, aggs Row, outer *EvalCtx) *EvalCtx {
	return &EvalCtx{Row: row, Aggs: aggs, Outer: outer, Vars: ex.frameVars, SubEval: ex.subEval,
		CallFunc: ex.callUserFunc, TrigTable: ex.trigTable, TrigNew: ex.trigNew, TrigOld: ex.trigOld}
}

func (ex *Executor) setIntoVar(name string, v Value) {
	if ex.frameVars == nil {
		fail("SELECT ... INTO can only be used inside a procedure, function, or trigger body")
	}
	ex.frameVars[name] = v
}

func tablePrefix(id uint32) []byte {
	var p []byte
	putU32(&p, id)
	return p
}

// ----------------------------------------------------------- permissions
func (ex *Executor) requireAdmin(what string) {
	if ex.CurUser != "admin" {
		fail("permission denied: %s requires the admin user", what)
	}
}

func (ex *Executor) checkPriv(table, priv string) {
	if ex.CurUser == "admin" {
		return
	}
	if !ex.cat.GrantsFor(ex.CurUser).has(table, priv) {
		fail("permission denied: user '%s' has no %s privilege on %s", ex.CurUser, priv, table)
	}
}

// --------------------------------------------------------------- Run
func (ex *Executor) Run(st *Stmt) Result {
	switch st.Kind {
	case SCreateTable:
		return ex.createTable(st)
	case SDropTable:
		return ex.dropTable(st)
	case SAlterTable:
		return ex.alterTable(st)
	case SCreateIndex:
		return ex.createIndex(st)
	case SDropIndex:
		return ex.dropIndex(st)
	case SInsert:
		return ex.insert(st)
	case SUpdate:
		return ex.update(st)
	case SDelete:
		return ex.del(st)
	case SShowTables:
		return ex.showTables()
	case SDescribe:
		return ex.describe(st)
	case SCreateView:
		return ex.createView(st)
	case SDropView:
		return ex.dropView(st)
	case SSelect:
		if !st.Explain {
			return ex.Select(st.Select, nil, nil)
		}
		plan := ex.explainPlan(&st.Select)
		r := Result{IsQuery: true, Columns: []string{"plan"}}
		for _, l := range plan {
			r.Rows = append(r.Rows, Row{VTextVal(l)})
		}
		return r
	case SCreateUser:
		return ex.createUser(st)
	case SDropUser:
		return ex.dropUser(st)
	case SGrant:
		return ex.grantStmt(st)
	case SRevoke:
		return ex.revokeStmt(st)
	case SShowGrants:
		return ex.showGrants(st.Name)
	case SCreateProc, SCreateFunc:
		return ex.createProc(st)
	case SDropProc, SDropFunc:
		return ex.dropProc(st)
	case SCreateTrigger:
		return ex.createTrigger(st)
	case SDropTrigger:
		return ex.dropTrigger(st)
	case SCall:
		_, r := ex.callProcedure(st.Name, st.CallArgs, nil)
		if r.IsQuery {
			return r
		}
		return r
	}
	fail("this statement must be run by the session, not the executor")
	return Result{}
}

// ----------------------------------------------------------- keys & rows
func (ex *Executor) rowKey(t *TableDef, r Row) string {
	k := tablePrefix(t.ID)
	for _, c := range t.PK {
		EncodeKeyPart(&k, r[c], t.Cols[c].Type)
	}
	return string(k)
}

func (ex *Executor) indexKey(t *TableDef, ix *IndexDef, r Row) string {
	k := tablePrefix(ix.ID)
	for _, c := range ix.Cols {
		EncodeKeyPart(&k, r[c], t.Cols[c].Type)
	}
	for _, c := range t.PK { // makes entries unique even for non-unique indexes
		EncodeKeyPart(&k, r[c], t.Cols[c].Type)
	}
	return string(k)
}

func (ex *Executor) loadRow(t *TableDef, val string) Row { return DecodeRow(val, len(t.Cols)) }

func (ex *Executor) insertRow(t *TableDef, r Row) {
	for i := range t.Cols {
		col := &t.Cols[i]
		if !CoerceToType(&r[i], col.Type) {
			fail("type mismatch for column '%s' (expected %s)", col.Name, col.Type)
		}
		if r[i].IsNull() && col.NotNull {
			fail("NOT NULL constraint failed: %s.%s", t.Name, col.Name)
		}
	}
	key := ex.rowKey(t, r)
	if len(key) > maxKeySize {
		fail("primary key too large")
	}
	if ex.tree.Has(key) {
		fail("UNIQUE constraint failed: primary key of %s", t.Name)
	}
	var ikeys []string
	for i := range t.Indexes {
		ix := &t.Indexes[i]
		ik := ex.indexKey(t, ix, r)
		if len(ik) > maxKeySize {
			fail("index key too large for index %s", ix.Name)
		}
		if ix.Unique {
			hasNull := false
			prefix := tablePrefix(ix.ID)
			for _, c := range ix.Cols {
				if r[c].IsNull() {
					hasNull = true
				}
				EncodeKeyPart(&prefix, r[c], t.Cols[c].Type)
			}
			if !hasNull { // SQL: NULLs never conflict in a UNIQUE index
				cur := ex.tree.Seek(string(prefix))
				if cur.Valid() && strings.HasPrefix(cur.Key(), string(prefix)) {
					fail("UNIQUE constraint failed: %s", ix.Name)
				}
			}
		}
		ikeys = append(ikeys, ik)
	}
	var val []byte
	EncodeRow(&val, r)
	if len(val) > maxValSize {
		fail("row too large (max %d bytes)", maxValSize)
	}
	ex.tree.Set(key, string(val))
	for _, ik := range ikeys {
		ex.tree.Set(ik, key)
	}
}

func (ex *Executor) deleteRow(t *TableDef, r StoredRow) {
	ex.tree.Del(r.Key)
	for i := range t.Indexes {
		ex.tree.Del(ex.indexKey(t, &t.Indexes[i], r.Vals))
	}
}

func (ex *Executor) alterTable(s *Stmt) Result {
	ex.requireAdmin("ALTER TABLE")
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		fail("no such table: %s", s.Table)
	}
	cd := s.AlterAdd
	if cd == nil {
		fail("unsupported ALTER TABLE operation")
	}
	if t.ColIndex(cd.Name) >= 0 {
		fail("duplicate column name: %s", cd.Name)
	}
	if cd.NotNull && cd.Default == nil {
		fail("cannot add NOT NULL column without DEFAULT")
	}
	// Materialize old rows before changing the schema.
	rows := ex.matchingRows(t, nil)
	nt := *t
	nt.Cols = append(append([]Column(nil), t.Cols...), Column{Name: cd.Name, Type: cd.Type, NotNull: cd.NotNull, Default: cd.Default})
	if cd.Unique {
		nt.Indexes = append(append([]IndexDef(nil), t.Indexes...), IndexDef{Name: nt.Name + "_" + cd.Name + "_unique", ID: ex.cat.AllocID(ex.tree), Unique: true, Cols: []int{len(nt.Cols) - 1}})
	} else {
		nt.Indexes = append([]IndexDef(nil), t.Indexes...)
	}
	// Remove old rows/indexes, then reinsert using the new schema.
	for _, r := range rows {
		ex.deleteRow(t, r)
	}
	*t = nt
	ex.cat.PutTable(ex.tree, t)
	for _, r := range rows {
		v := VNullVal()
		if cd.Default != nil {
			v = evalExpr(cd.Default, ex.ctx(nil, nil))
		}
		r.Vals = append(r.Vals, v)
		ex.insertRow(t, r.Vals)
	}
	return msgResult("ALTER TABLE", int64(len(rows)))
}

// --------------------------------------------------------------- planner
type colCond struct {
	col int
	op  string
	v   Value
}

type AccessPath struct {
	Lo, Hi string
	Index  *IndexDef // nil = primary key
	Desc   string
}

func flattenAnd(e *Expr, out *[]*Expr) {
	if e == nil {
		return
	}
	if e.Kind == EKBinary && e.Op == "AND" {
		flattenAnd(e.Args[0], out)
		flattenAnd(e.Args[1], out)
	} else {
		*out = append(*out, e)
	}
}

func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

func (ex *Executor) plan(t *TableDef, alias string, where *Expr) AccessPath {
	var conj []*Expr
	flattenAnd(where, &conj)
	var conds []colCond
	for _, c := range conj {
		if c.Kind != EKBinary {
			continue
		}
		op := c.Op
		if op != "=" && op != "<" && op != "<=" && op != ">" && op != ">=" {
			continue
		}
		a, b := c.Args[0], c.Args[1]
		if a.Kind == EKLit && b.Kind == EKCol {
			a, b = b, a
			op = flipOp(op)
		}
		if a.Kind != EKCol || b.Kind != EKLit || b.Lit.IsNull() {
			continue
		}
		if a.Table != "" && a.Table != alias {
			continue
		}
		ci := t.ColIndex(a.Name)
		if ci < 0 {
			continue
		}
		conds = append(conds, colCond{col: ci, op: op, v: b.Lit})
	}
	enc := func(cd colCond, out *[]byte) bool {
		v := cd.v
		if !CoerceToType(&v, t.Cols[cd.col].Type) {
			return false
		}
		EncodeKeyPart(out, v, t.Cols[cd.col].Type)
		return true
	}
	build := func(ix *IndexDef, cols []int, id uint32) (AccessPath, int) {
		prefix := tablePrefix(id)
		var label strings.Builder
		eq := 0
		for _, c := range cols {
			ok := false
			for _, cd := range conds {
				if cd.col != c || cd.op != "=" {
					continue
				}
				tmp := append([]byte(nil), prefix...)
				if enc(cd, &tmp) {
					prefix = tmp
					ok = true
					break
				}
			}
			if !ok {
				break
			}
			if eq > 0 {
				label.WriteString(", ")
			}
			label.WriteString(t.Cols[c].Name + "=?")
			eq++
		}
		ap := AccessPath{Index: ix, Lo: string(prefix), Hi: string(PrefixSuccessor(prefix))}
		score := eq * 2
		if eq < len(cols) {
			c := cols[eq]
			ranged := false
			for _, cd := range conds {
				if cd.col != c || cd.op == "=" {
					continue
				}
				k := append([]byte(nil), prefix...)
				if !enc(cd, &k) {
					continue
				}
				switch cd.op {
				case ">=":
					ap.Lo = string(k)
				case ">":
					ap.Lo = string(PrefixSuccessor(k))
				case "<":
					ap.Hi = string(k)
				default: // <=
					ap.Hi = string(PrefixSuccessor(k))
				}
				ranged = true
			}
			if ranged {
				if eq > 0 {
					label.WriteString(", ")
				}
				label.WriteString(t.Cols[c].Name + " range")
				score++
			}
		}
		ixDesc := "PRIMARY KEY"
		if ix != nil {
			ixDesc = "INDEX " + ix.Name
		}
		ap.Desc = "SEARCH " + t.Name + " USING " + ixDesc + " (" + label.String() + ")"
		return ap, score
	}
	best, bestScore := build(nil, t.PK, t.ID)
	for i := range t.Indexes {
		ix := &t.Indexes[i]
		ap, sc := build(ix, ix.Cols, ix.ID)
		if sc > bestScore {
			bestScore, best = sc, ap
		}
	}
	if bestScore == 0 {
		best.Desc = "SCAN " + t.Name
	}
	return best
}

// scan walks the access path, calling cb for each row. cb must not modify
// the tree while the cursor is live -- callers that mutate rows collect
// them into a slice first.
func (ex *Executor) scan(t *TableDef, ap AccessPath, cb func(StoredRow)) {
	for cur := ex.tree.Seek(ap.Lo); cur.Valid() && cur.Key() < ap.Hi; cur.Next() {
		var sr StoredRow
		if ap.Index == nil {
			sr.Key = cur.Key()
			sr.Vals = ex.loadRow(t, cur.Val())
		} else {
			sr.Key = cur.Val()
			v, ok := ex.tree.Get(sr.Key)
			if !ok {
				fail("corrupt index: entry points to a missing row")
			}
			sr.Vals = ex.loadRow(t, v)
		}
		cb(sr)
	}
}

func (ex *Executor) matchingRows(t *TableDef, where *Expr) []StoredRow {
	sc := make(Schema, len(t.Cols))
	for i, c := range t.Cols {
		sc[i] = SchemaCol{Table: t.Name, Name: c.Name}
	}
	if where != nil {
		bindExpr(where, sc, nil)
		if containsAgg(where) {
			fail("aggregate functions are not allowed in WHERE")
		}
	}
	ap := ex.plan(t, t.Name, where)
	var out []StoredRow
	ex.scan(t, ap, func(r StoredRow) {
		if where != nil {
			ctx := ex.ctx(r.Vals, nil)
			if truth(evalExpr(where, ctx)) != 1 {
				return
			}
		}
		out = append(out, r)
	})
	return out
}

// ------------------------------------------------------------------- DDL
func (ex *Executor) createTable(s *Stmt) Result {
	ex.requireAdmin("CREATE TABLE")
	if _, ok := ex.cat.Tables[s.Table]; ok {
		if s.IfNotExists {
			return msgResult("table already exists", 0)
		}
		fail("table already exists: %s", s.Table)
	}
	def := &TableDef{Name: s.Table}
	var colPk []int
	for _, cd := range s.ColDefs {
		if def.ColIndex(cd.Name) >= 0 {
			fail("duplicate column name: %s", cd.Name)
		}
		def.Cols = append(def.Cols, Column{Name: cd.Name, Type: cd.Type, NotNull: cd.NotNull, Default: cd.Default})
		if cd.PK {
			colPk = append(colPk, len(def.Cols)-1)
		}
	}
	if len(def.Cols) == 0 {
		fail("a table needs at least one column")
	}
	if len(s.PkCols) > 0 && len(colPk) > 0 {
		fail("multiple PRIMARY KEY definitions")
	}
	if len(colPk) > 1 {
		fail("multiple PRIMARY KEY columns: use PRIMARY KEY (a, b)")
	}
	def.PK = colPk
	for _, n := range s.PkCols {
		ci := def.ColIndex(n)
		if ci < 0 {
			fail("PRIMARY KEY refers to unknown column: %s", n)
		}
		def.PK = append(def.PK, ci)
	}
	if len(def.PK) == 0 {
		fail("table needs a PRIMARY KEY (this engine stores rows in primary-key order)")
	}
	for _, p := range def.PK {
		def.Cols[p].NotNull = true
	}
	def.ID = ex.cat.AllocID(ex.tree)
	addUnique := func(cols []int) {
		name := def.Name
		for _, c := range cols {
			name += "_" + def.Cols[c].Name
		}
		name += "_unique"
		def.Indexes = append(def.Indexes, IndexDef{Name: name, ID: ex.cat.AllocID(ex.tree), Unique: true, Cols: cols})
	}
	for i, cd := range s.ColDefs {
		if cd.Unique {
			addUnique([]int{i})
		}
	}
	for _, u := range s.Uniques {
		var cols []int
		for _, n := range u {
			ci := def.ColIndex(n)
			if ci < 0 {
				fail("UNIQUE refers to unknown column: %s", n)
			}
			cols = append(cols, ci)
		}
		addUnique(cols)
	}
	ex.cat.PutTable(ex.tree, def)
	return msgResult("CREATE TABLE", 0)
}

func (ex *Executor) dropTable(s *Stmt) Result {
	ex.requireAdmin("DROP TABLE")
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		if s.IfExists {
			return msgResult("no such table", 0)
		}
		fail("no such table: %s", s.Table)
	}
	ids := []uint32{t.ID}
	for _, ix := range t.Indexes {
		ids = append(ids, ix.ID)
	}
	for _, id := range ids {
		lo := string(tablePrefix(id))
		hi := string(PrefixSuccessor(tablePrefix(id)))
		var keys []string
		for cur := ex.tree.Seek(lo); cur.Valid() && cur.Key() < hi; cur.Next() {
			keys = append(keys, cur.Key())
		}
		for _, k := range keys {
			ex.tree.Del(k)
		}
	}
	ex.cat.RemoveTable(ex.tree, s.Table)
	return msgResult("DROP TABLE", 0)
}

func (ex *Executor) createIndex(s *Stmt) Result {
	ex.requireAdmin("CREATE INDEX")
	for _, t := range ex.cat.Tables {
		for _, ix := range t.Indexes {
			if ix.Name == s.Name {
				if s.IfNotExists {
					return msgResult("index already exists", 0)
				}
				fail("index already exists: %s", s.Name)
			}
		}
	}
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		fail("no such table: %s", s.Table)
	}
	tCopy := *t
	ix := IndexDef{Name: s.Name, Unique: s.Unique}
	for _, n := range s.IdxCols {
		ci := tCopy.ColIndex(n)
		if ci < 0 {
			fail("no such column: %s", n)
		}
		ix.Cols = append(ix.Cols, ci)
	}
	ix.ID = ex.cat.AllocID(ex.tree)
	full := ex.plan(&tCopy, tCopy.Name, nil)
	var rows []StoredRow
	ex.scan(&tCopy, full, func(r StoredRow) { rows = append(rows, r) })
	tCopy.Indexes = append(tCopy.Indexes, ix)
	added := &tCopy.Indexes[len(tCopy.Indexes)-1]
	seenUnique := map[string]bool{}
	for _, r := range rows {
		if added.Unique {
			var u []byte
			hasNull := false
			for _, c := range added.Cols {
				if r.Vals[c].IsNull() {
					hasNull = true
				}
				EncodeKeyPart(&u, r.Vals[c], tCopy.Cols[c].Type)
			}
			if !hasNull {
				if seenUnique[string(u)] {
					fail("UNIQUE constraint failed: existing rows contain duplicates for %s", s.Name)
				}
				seenUnique[string(u)] = true
			}
		}
		ik := ex.indexKey(&tCopy, added, r.Vals)
		if len(ik) > maxKeySize {
			fail("index key too large")
		}
		ex.tree.Set(ik, r.Key)
	}
	ex.cat.PutTable(ex.tree, &tCopy)
	return msgResult("CREATE INDEX", int64(len(rows)))
}

func (ex *Executor) dropIndex(s *Stmt) Result {
	ex.requireAdmin("DROP INDEX")
	for _, t := range ex.cat.Tables {
		tCopy := *t
		for i, ix := range tCopy.Indexes {
			if ix.Name != s.Name {
				continue
			}
			lo := string(tablePrefix(ix.ID))
			hi := string(PrefixSuccessor(tablePrefix(ix.ID)))
			var keys []string
			for cur := ex.tree.Seek(lo); cur.Valid() && cur.Key() < hi; cur.Next() {
				keys = append(keys, cur.Key())
			}
			for _, k := range keys {
				ex.tree.Del(k)
			}
			tCopy.Indexes = append(append([]IndexDef{}, tCopy.Indexes[:i]...), tCopy.Indexes[i+1:]...)
			ex.cat.PutTable(ex.tree, &tCopy)
			return msgResult("DROP INDEX", 0)
		}
	}
	if s.IfExists {
		return msgResult("no such index", 0)
	}
	fail("no such index: %s", s.Name)
	return Result{}
}

func (ex *Executor) createView(s *Stmt) Result {
	ex.requireAdmin("CREATE VIEW")
	if _, ok := ex.cat.Views[s.Name]; ok && !s.IfExists {
		if s.IfNotExists {
			return msgResult("view already exists", 0)
		}
		fail("view already exists: %s", s.Name)
	}
	ex.cat.PutView(ex.tree, &ViewDef{Name: s.Name, Query: s.RawText})
	return msgResult("CREATE VIEW", 0)
}

func (ex *Executor) dropView(s *Stmt) Result {
	ex.requireAdmin("DROP VIEW")
	if _, ok := ex.cat.Views[s.Name]; !ok {
		if s.IfExists {
			return msgResult("no such view", 0)
		}
		fail("no such view: %s", s.Name)
	}
	ex.cat.RemoveView(ex.tree, s.Name)
	return msgResult("DROP VIEW", 0)
}

func (ex *Executor) showTables() Result {
	r := Result{IsQuery: true, Columns: []string{"table", "columns", "indexes"}}
	var names []string
	for n := range ex.cat.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := ex.cat.Tables[n]
		var ixs []string
		for _, ix := range t.Indexes {
			ixs = append(ixs, ix.Name)
		}
		r.Rows = append(r.Rows, Row{VTextVal(n), VIntVal(int64(len(t.Cols))), VTextVal(strings.Join(ixs, ", "))})
	}
	var vnames []string
	for n := range ex.cat.Views {
		vnames = append(vnames, n)
	}
	sort.Strings(vnames)
	for _, n := range vnames {
		r.Rows = append(r.Rows, Row{VTextVal(n + " (view)"), VIntVal(0), VTextVal("")})
	}
	return r
}

func (ex *Executor) describe(s *Stmt) Result {
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		fail("no such table: %s", s.Table)
	}
	r := Result{IsQuery: true, Columns: []string{"column", "type", "null", "key"}}
	for i, c := range t.Cols {
		pk := false
		for _, p := range t.PK {
			if p == i {
				pk = true
			}
		}
		key := ""
		if pk {
			key = "PRI"
		}
		for _, ix := range t.Indexes {
			for _, c2 := range ix.Cols {
				if c2 == i && !pk {
					if ix.Unique {
						key = "UNI"
					} else {
						key = "IDX"
					}
				}
			}
		}
		nullable := "YES"
		if c.NotNull {
			nullable = "NO"
		}
		r.Rows = append(r.Rows, Row{VTextVal(c.Name), VTextVal(c.Type.String()), VTextVal(nullable), VTextVal(key)})
	}
	return r
}

// ------------------------------------------------------------------- DML
func (ex *Executor) insert(s *Stmt) Result {
	ex.checkPriv(s.Table, "INSERT")
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		fail("no such table: %s", s.Table)
	}
	var colMap []int
	if len(s.InsertCols) == 0 {
		for i := range t.Cols {
			colMap = append(colMap, i)
		}
	} else {
		for _, n := range s.InsertCols {
			ci := t.ColIndex(n)
			if ci < 0 {
				fail("no such column: %s", n)
			}
			for _, m := range colMap {
				if m == ci {
					fail("column listed twice: %s", n)
				}
			}
			colMap = append(colMap, ci)
		}
	}
	buildRow := func(vals []Value) Row {
		row := make(Row, len(t.Cols))
		for i, c := range t.Cols {
			if c.Default != nil {
				row[i] = evalExpr(c.Default, ex.ctx(nil, nil))
			}
		}
		for i, v := range vals {
			row[colMap[i]] = v
		}
		return row
	}
	n := int64(0)
	if s.InsertSel != nil {
		res := ex.Select(*s.InsertSel, nil, nil)
		for _, srcRow := range res.Rows {
			if len(srcRow) != len(colMap) {
				fail("INSERT has %d columns but SELECT returns %d", len(colMap), len(srcRow))
			}
			row := buildRow(srcRow)
			ex.fireTriggers("BEFORE", "INSERT", t, nil, &row)
			ex.insertRow(t, row)
			ex.fireTriggers("AFTER", "INSERT", t, nil, &row)
			n++
		}
		return msgResult("INSERT "+itoa(n), n)
	}
	for _, exprs := range s.Rows {
		if len(exprs) != len(colMap) {
			fail("expected %d values but got %d", len(colMap), len(exprs))
		}
		vals := make([]Value, len(exprs))
		for i, e := range exprs {
			bindExpr(e, nil, nil)
			vals[i] = evalExpr(e, ex.ctx(nil, nil))
		}
		row := buildRow(vals)
		ex.fireTriggers("BEFORE", "INSERT", t, nil, &row)
		ex.insertRow(t, row)
		ex.fireTriggers("AFTER", "INSERT", t, nil, &row)
		n++
	}
	return msgResult("INSERT "+itoa(n), n)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (ex *Executor) update(s *Stmt) Result {
	ex.checkPriv(s.Table, "UPDATE")
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		fail("no such table: %s", s.Table)
	}
	sc := make(Schema, len(t.Cols))
	for i, c := range t.Cols {
		sc[i] = SchemaCol{Table: t.Name, Name: c.Name}
	}
	var targets []int
	for i := range s.Sets {
		ci := t.ColIndex(s.Sets[i].Col)
		if ci < 0 {
			fail("no such column: %s", s.Sets[i].Col)
		}
		bindExpr(s.Sets[i].Value, sc, nil)
		targets = append(targets, ci)
	}
	rows := ex.matchingRows(t, s.Where) // materialised first (Halloween problem)
	for _, old := range rows {
		nv := append(Row(nil), old.Vals...)
		ctx := ex.ctx(old.Vals, nil)
		for i := range s.Sets {
			nv[targets[i]] = evalExpr(s.Sets[i].Value, ctx)
		}
		ex.fireTriggers("BEFORE", "UPDATE", t, &old.Vals, &nv)
		ex.deleteRow(t, old)
		ex.insertRow(t, nv)
		ex.fireTriggers("AFTER", "UPDATE", t, &old.Vals, &nv)
	}
	return msgResult("UPDATE "+itoa(int64(len(rows))), int64(len(rows)))
}

func (ex *Executor) del(s *Stmt) Result {
	ex.checkPriv(s.Table, "DELETE")
	t, ok := ex.cat.Tables[s.Table]
	if !ok {
		fail("no such table: %s", s.Table)
	}
	rows := ex.matchingRows(t, s.Where)
	for _, r := range rows {
		ex.fireTriggers("BEFORE", "DELETE", t, &r.Vals, nil)
		ex.deleteRow(t, r)
		ex.fireTriggers("AFTER", "DELETE", t, &r.Vals, nil)
	}
	return msgResult("DELETE "+itoa(int64(len(rows))), int64(len(rows)))
}
