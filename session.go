package main

import "os"

type Session struct {
	path      string
	pager     *Pager
	tree      *BTree
	cat       *Catalog
	ex        *Executor
	inTxn     bool
	catBackup *Catalog
	prepared  map[string]string // name -> raw SQL text
}

func NewSession(path string) (*Session, error) {
	pager, err := OpenPager(path)
	if err != nil {
		return nil, err
	}
	tree := NewBTree(pager)
	cat := NewCatalog()
	cat.Load(tree)
	ex := NewExecutor(tree, cat)
	return &Session{path: path, pager: pager, tree: tree, cat: cat, ex: ex, prepared: map[string]string{}}, nil
}

func (s *Session) Close() error { return s.pager.Close() }

func (s *Session) setCat(c *Catalog) {
	s.cat = c
	s.ex.cat = c
}

func (s *Session) InTransaction() bool   { return s.inTxn }
func (s *Session) PageCount() uint64     { return s.pager.PageCount() }
func (s *Session) FreePageCount() uint64 { return s.pager.FreePageCount() }
func (s *Session) Verify() int           { return s.tree.Verify() }
func (s *Session) CurrentUser() string   { return s.ex.CurUser }

func (s *Session) SetUser(name string) error {
	if _, ok := s.cat.Users[name]; !ok {
		return dbErrorf("no such user: %s", name)
	}
	s.ex.CurUser = name
	return nil
}

// Execute runs one already-parsed statement, recovering any DBError raised
// during planning/execution into a normal Go error.
func (s *Session) Execute(st *Stmt) (res Result, err error) {
	defer catchDBError(&err)
	switch st.Kind {
	case SCreateDatabase:
		if st.Database == "" {
			fail("database name required")
		}
		p := st.Database + ".db"
		pager, e := OpenPager(p)
		if e != nil {
			return Result{}, e
		}
		pager.Close()
		return msgResult("CREATE DATABASE", 0), nil
	case SUseDatabase:
		if s.inTxn {
			fail("cannot switch database during a transaction")
		}
		p := st.Database + ".db"
		if _, e := os.Stat(p); e != nil {
			fail("no such database: %s", st.Database)
		}
		pager, e := OpenPager(p)
		if e != nil {
			fail("cannot open database %s: %v", st.Database, e)
		}
		old := s.pager
		s.pager = pager
		s.tree = NewBTree(pager)
		s.cat = NewCatalog()
		s.cat.Load(s.tree)
		s.ex.tree, s.ex.cat = s.tree, s.cat
		s.ex.CurUser = "admin"
		s.prepared = map[string]string{}
		s.path = p
		old.Close()
		return msgResult("USE "+st.Database, 0), nil
	case SBegin:
		if s.inTxn {
			fail("a transaction is already open")
		}
		s.inTxn = true
		s.catBackup = s.cat.clone()
		return msgResult("BEGIN", 0), nil
	case SCommit:
		if !s.inTxn {
			fail("no transaction is open")
		}
		s.pager.Commit()
		s.inTxn = false
		return msgResult("COMMIT", 0), nil
	case SRollback:
		if !s.inTxn {
			fail("no transaction is open")
		}
		s.pager.Rollback()
		s.setCat(s.catBackup)
		s.inTxn = false
		return msgResult("ROLLBACK", 0), nil
	case SPrepare:
		if _, dup := s.prepared[st.Name]; dup {
			fail("prepared statement already exists: %s", st.Name)
		}
		// Validate it parses now, so errors surface at PREPARE time.
		if len(ParseSQL(st.PrepSQL)) != 1 {
			fail("a prepared statement must contain exactly one SQL statement")
		}
		s.prepared[st.Name] = st.PrepSQL
		return msgResult("PREPARE", 0), nil
	case SDeallocate:
		if _, ok := s.prepared[st.Name]; !ok {
			fail("no such prepared statement: %s", st.Name)
		}
		delete(s.prepared, st.Name)
		return msgResult("DEALLOCATE", 0), nil
	case SExecute:
		sqlText, ok := s.prepared[st.Name]
		if !ok {
			fail("no such prepared statement: %s", st.Name)
		}
		inner := ParseSQL(sqlText)[0]
		args := make([]Value, len(st.ExecArgs))
		for i, e := range st.ExecArgs {
			bindExpr(e, nil, nil)
			args[i] = evalExpr(e, s.ex.ctx(nil, nil))
		}
		substituteParams(&inner, args)
		return s.runOneAutocommit(&inner)
	case SExecuteImmediate:
		v := evalExpr(st.CallArgs[0], s.ex.ctx(nil, nil))
		if v.Kind != VText {
			fail("EXECUTE IMMEDIATE requires a text expression")
		}
		stmts := ParseSQL(v.S)
		if len(stmts) != 1 {
			fail("EXECUTE IMMEDIATE requires exactly one SQL statement")
		}
		return s.runOneAutocommit(&stmts[0])
	default:
		return s.runOneAutocommit(st)
	}
}

// runOneAutocommit runs a DDL/DML/SELECT/CALL/DCL/DO-block statement.
// Outside an explicit transaction it commits on its own (autocommit) and
// rolls back cleanly on error; inside BEGIN..COMMIT, nothing hits disk
// until COMMIT, and an error only undoes this one statement.
func (s *Session) runOneAutocommit(st *Stmt) (res Result, err error) {
	own := !s.inTxn
	sp := s.pager.save()
	catSp := s.cat.clone()
	defer func() {
		if r := recover(); r != nil {
			s.pager.Restore(sp)
			s.setCat(catSp)
			panic(r)
		}
	}()
	if st.Kind == SDoBlock {
		res = s.ex.RunDoBlock(st.Body)
	} else {
		res = s.ex.Run(st)
	}
	if own {
		s.pager.Commit()
	}
	return res, nil
}

// ---- dynamic SQL: substituting ? placeholders in a parsed statement.
func walkExprTree(e *Expr, fn func(*Expr)) {
	if e == nil {
		return
	}
	fn(e)
	for _, a := range e.Args {
		walkExprTree(a, fn)
	}
	walkExprTree(e.CaseOperand, fn)
	for _, w := range e.WhenConds {
		walkExprTree(w, fn)
	}
	for _, w := range e.WhenVals {
		walkExprTree(w, fn)
	}
	walkExprTree(e.ElseVal, fn)
	if e.Sub != nil {
		walkSelectExprs(e.Sub, fn)
	}
}

func walkSelectExprs(q *SelectStmt, fn func(*Expr)) {
	if q == nil {
		return
	}
	for i := range q.Items {
		if q.Items[i].Expr != nil {
			walkExprTree(q.Items[i].Expr, fn)
		}
	}
	walkExprTree(q.Where, fn)
	for _, g := range q.GroupBy {
		walkExprTree(g, fn)
	}
	walkExprTree(q.Having, fn)
	for _, o := range q.OrderBy {
		walkExprTree(o.Expr, fn)
	}
	for i := range q.Joins {
		walkExprTree(q.Joins[i].On, fn)
		if q.Joins[i].Ref.Sub != nil {
			walkSelectExprs(q.Joins[i].Ref.Sub, fn)
		}
	}
	if q.From.Sub != nil {
		walkSelectExprs(q.From.Sub, fn)
	}
	if q.Combined != nil {
		walkSelectExprs(q.Combined, fn)
	}
}

func walkStmtExprs(st *Stmt, fn func(*Expr)) {
	walkExprTree(st.Where, fn)
	for i := range st.Sets {
		walkExprTree(st.Sets[i].Value, fn)
	}
	for _, row := range st.Rows {
		for _, e := range row {
			walkExprTree(e, fn)
		}
	}
	if st.InsertSel != nil {
		walkSelectExprs(st.InsertSel, fn)
	}
	walkSelectExprs(&st.Select, fn)
	for _, a := range st.CallArgs {
		walkExprTree(a, fn)
	}
}

// substituteParams replaces every ? placeholder in st, in left-to-right
// occurrence order, with the corresponding literal from args.
func substituteParams(st *Stmt, args []Value) {
	idx := 0
	walkStmtExprs(st, func(e *Expr) {
		if e.Kind != EKParam {
			return
		}
		if idx >= len(args) {
			fail("not enough arguments for prepared statement (expected more than %d)", len(args))
		}
		e.Kind = EKLit
		e.Lit = args[idx]
		idx++
	})
	if idx != len(args) {
		fail("prepared statement takes %d parameter(s) but %d were given", idx, len(args))
	}
}
