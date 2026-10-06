// parser.go - recursive-descent parser for the whole SQL dialect.
package main

import (
	"strconv"
	"strings"
)

type Parser struct {
	t   []Token
	p   int
	src string 
}

func newParser(toks []Token) *Parser { return &Parser{t: toks} }

func newParserSrc(src string) *Parser { return &Parser{t: lex(src), src: src} }

func (p *Parser) rawSlice(from int) string {
	to := p.cur().Pos
	if to > len(p.src) {
		to = len(p.src)
	}
	return strings.TrimSpace(p.src[from:to])
}

func (p *Parser) cur() Token { return p.t[p.p] }
func (p *Parser) peek(k int) Token {
	idx := p.p + k
	if idx >= len(p.t) {
		idx = len(p.t) - 1
	}
	return p.t[idx]
}
func (p *Parser) advance() {
	if p.p+1 < len(p.t) {
		p.p++
	}
}

func (p *Parser) failNear(msg string) {
	near := "end of input"
	if p.cur().Kind != TkEnd {
		near = "'" + p.cur().Text + "'"
	}
	fail("syntax error near %s: %s", near, msg)
}

func (p *Parser) kw(w string) bool { return p.cur().Kind == TkIdent && p.cur().Text == w }
func (p *Parser) acceptKw(w string) bool {
	if p.kw(w) {
		p.advance()
		return true
	}
	return false
}
func (p *Parser) expectKw(w string) {
	if !p.acceptKw(w) {
		p.failNear("expected " + strings.ToUpper(w))
	}
}
func (p *Parser) sym(s string) bool { return p.cur().Kind == TkSymbol && p.cur().Text == s }
func (p *Parser) acceptSym(s string) bool {
	if p.sym(s) {
		p.advance()
		return true
	}
	return false
}
func (p *Parser) expectSym(s string) {
	if !p.acceptSym(s) {
		p.failNear("expected '" + s + "'")
	}
}

func (p *Parser) ident(what string) string {
	if p.cur().Kind != TkIdent {
		p.failNear("expected " + what)
	}
	s := p.cur().Text
	p.advance()
	return s
}

func (p *Parser) intLit() int64 {
	if p.cur().Kind != TkNumber || p.cur().IsReal {
		p.failNear("expected integer")
	}
	v, err := strconv.ParseInt(p.cur().Text, 10, 64)
	if err != nil {
		p.failNear("integer out of range")
	}
	p.advance()
	return v
}

func (p *Parser) identList() []string {
	var v []string
	p.expectSym("(")
	for {
		v = append(v, p.ident("column name"))
		if !p.acceptSym(",") {
			break
		}
	}
	p.expectSym(")")
	return v
}

// ------------------------------------------------------------- top level
func ParseSQL(sql string) []Stmt {
	p := newParserSrc(sql)
	var out []Stmt
	for {
		for p.acceptSym(";") {
		}
		if p.cur().Kind == TkEnd {
			break
		}
		out = append(out, p.statement())
		if p.cur().Kind != TkEnd && !p.sym(";") {
			p.failNear("expected ';' or end of input")
		}
	}
	return out
}

type SQLScript struct{ p *Parser }

func NewSQLScript(sql string) *SQLScript { return &SQLScript{p: newParserSrc(sql)} }

func ParseSelectText(sql string) SelectStmt {
	p := newParserSrc(sql)
	return p.selectWithSetOps()
}

// ParseBodyText parses a procedural block's statements (the text that was
// captured between BEGIN and END), used to re-parse a stored
// procedure/function/trigger body on each call.
func ParseBodyText(sql string) []PStmt {
	p := newParserSrc(sql)
	body := p.stmtBlockUntilEnd()
	if p.cur().Kind != TkEnd {
		p.failNear("unexpected token in procedure/trigger body")
	}
	return body
}

func (s *SQLScript) Done() bool {
	for s.p.acceptSym(";") {
	}
	return s.p.cur().Kind == TkEnd
}

func (s *SQLScript) Next() (st Stmt, err error) {
	// A single defer does recover + error conversion + position recovery,
	// in that order. (Two separate defers is a trap here: defers run in
	// LIFO order, so a defer that merely *checks* the named err result
	// would run before the defer that sets it via recover(), and would
	// always see it as nil — meaning recoverPastStatement would never
	// run and the parser would get stuck replaying the same failing
	// token forever.)
	defer func() {
		if r := recover(); r != nil {
			de, ok := r.(*DBError)
			if !ok {
				panic(r)
			}
			err = de
			s.p.recoverPastStatement()
		}
	}()
	st = s.p.statement()
	if s.p.cur().Kind != TkEnd && !s.p.sym(";") {
		s.p.failNear("expected ';' or end of input")
	}
	s.p.acceptSym(";")
	return st, nil
}

func (p *Parser) recoverPastStatement() {
	depth := 0
	for p.cur().Kind != TkEnd {
		if p.sym("(") {
			depth++
		} else if p.sym(")") {
			if depth > 0 {
				depth--
			}
		} else if p.sym(";") && depth == 0 {
			break
		}
		p.advance()
	}
	p.acceptSym(";")
}

// ------------------------------------------------------------- statement
func (p *Parser) statement() Stmt {
	if p.acceptKw("explain") {
		s := p.statement()
		if s.Kind != SSelect {
			p.failNear("EXPLAIN supports SELECT only")
		}
		s.Explain = true
		return s
	}
	switch {
	case p.acceptKw("create"):
		return p.create()
	case p.acceptKw("drop"):
		return p.drop()
	case p.acceptKw("alter"):
		return p.alter()
	case p.acceptKw("insert"):
		return p.insert()
	case p.kw("select"):
		return Stmt{Kind: SSelect, Select: p.selectWithSetOps()}
	case p.acceptKw("update"):
		return p.update()
	case p.acceptKw("delete"):
		return p.del()
	case p.acceptKw("begin"):
		p.acceptKw("transaction")
		return Stmt{Kind: SBegin}
	case p.acceptKw("commit"):
		p.acceptKw("transaction")
		return Stmt{Kind: SCommit}
	case p.acceptKw("use"):
		return Stmt{Kind: SUseDatabase, Database: p.ident("database name")}
	case p.acceptKw("rollback"):
		p.acceptKw("transaction")
		return Stmt{Kind: SRollback}
	case p.acceptKw("show"):
		if p.acceptKw("tables") {
			return Stmt{Kind: SShowTables}
		}
		p.expectKw("grants")
		name := ""
		if p.acceptKw("for") {
			name = p.ident("user name")
		}
		return Stmt{Kind: SShowGrants, Name: name}
	case p.acceptKw("describe"), p.acceptKw("desc"):
		return Stmt{Kind: SDescribe, Table: p.ident("table name")}
	case p.acceptKw("call"):
		name := p.ident("procedure name")
		var args []*Expr
		if p.acceptSym("(") {
			if !p.sym(")") {
				for {
					args = append(args, p.expr())
					if !p.acceptSym(",") {
						break
					}
				}
			}
			p.expectSym(")")
		}
		return Stmt{Kind: SCall, Name: name, CallArgs: args}
	case p.acceptKw("grant"):
		return p.grant()
	case p.acceptKw("revoke"):
		return p.revoke()
	case p.acceptKw("prepare"):
		name := p.ident("prepared statement name")
		p.expectKw("from")
		sqlLit := p.stringLit("SQL text")
		return Stmt{Kind: SPrepare, Name: name, PrepSQL: sqlLit}
	case p.acceptKw("execute"):
		if p.acceptKw("immediate") {
			sqlExpr := p.expr()
			return Stmt{Kind: SExecuteImmediate, CallArgs: []*Expr{sqlExpr}}
		}
		name := p.ident("prepared statement name")
		var args []*Expr
		if p.acceptKw("using") {
			for {
				args = append(args, p.expr())
				if !p.acceptSym(",") {
					break
				}
			}
		}
		return Stmt{Kind: SExecute, Name: name, ExecArgs: args}
	case p.acceptKw("deallocate"):
		p.acceptKw("prepare")
		name := p.ident("prepared statement name")
		return Stmt{Kind: SDeallocate, Name: name}
	case p.acceptKw("do"):
		p.expectKw("begin")
		body := p.stmtBlockUntilEnd()
		p.expectKw("end")
		return Stmt{Kind: SDoBlock, Body: body}
	}
	p.failNear("unknown statement")
	panic("unreachable")
}

func (p *Parser) stringLit(what string) string {
	if p.cur().Kind != TkString {
		p.failNear("expected " + what)
	}
	s := p.cur().Text
	p.advance()
	return s
}

// ------------------------------------------------------------------ types
func (p *Parser) parseType() ColType {
	t := p.ident("type name")
	var r ColType
	switch t {
	case "int", "integer", "bigint", "smallint", "serial":
		r = TInt
	case "real", "double", "float", "numeric", "decimal":
		r = TReal
		if t == "double" {
			p.acceptKw("precision")
		}
	case "text", "varchar", "char", "string", "date", "datetime", "timestamp":
		r = TText
	case "bool", "boolean":
		r = TBool
	default:
		p.failNear("unknown type '" + t + "'")
	}
	if p.acceptSym("(") { // VARCHAR(20), NUMERIC(10,2): sizes accepted, ignored
		p.intLit()
		if p.acceptSym(",") {
			p.intLit()
		}
		p.expectSym(")")
	}
	return r
}

// -------------------------------------------------------------- CREATE
func (p *Parser) create() Stmt {
	unique := p.acceptKw("unique")
	switch {
	case p.acceptKw("index"):
		s := Stmt{Kind: SCreateIndex, Unique: unique}
		if p.acceptKw("if") {
			p.expectKw("not")
			p.expectKw("exists")
			s.IfNotExists = true
		}
		s.Name = p.ident("index name")
		p.expectKw("on")
		s.Table = p.ident("table name")
		s.IdxCols = p.identList()
		return s
	case unique:
		p.failNear("expected INDEX")
	case p.acceptKw("database"):
		st := Stmt{Kind: SCreateDatabase}
		st.Database = p.ident("database name")
		return st
	case p.acceptKw("table"):
		return p.createTable()
	case p.acceptKw("view"):
		s := Stmt{Kind: SCreateView}
		if p.acceptKw("if") {
			p.expectKw("not")
			p.expectKw("exists")
			s.IfNotExists = true
		}
		s.Name = p.ident("view name")
		p.expectKw("as")
		start := p.cur().Pos
		s.Select = p.selectWithSetOps()
		s.RawText = p.rawSlice(start)
		return s
	case p.acceptKw("or"):
		p.expectKw("replace")
		if p.acceptKw("view") {
			s := Stmt{Kind: SCreateView, IfNotExists: false}
			s.Name = p.ident("view name")
			s.IfExists = true // signals "replace if exists" to the executor
			p.expectKw("as")
			start := p.cur().Pos
			s.Select = p.selectWithSetOps()
			s.RawText = p.rawSlice(start)
			return s
		}
		if p.acceptKw("procedure") {
			return p.createProcOrFunc(false, true)
		}
		if p.acceptKw("function") {
			return p.createProcOrFunc(true, true)
		}
		p.failNear("expected VIEW, PROCEDURE or FUNCTION after OR REPLACE")
	case p.acceptKw("procedure"):
		return p.createProcOrFunc(false, false)
	case p.acceptKw("function"):
		return p.createProcOrFunc(true, false)
	case p.acceptKw("trigger"):
		return p.createTrigger()
	case p.acceptKw("user"):
		s := Stmt{Kind: SCreateUser}
		s.Name = p.ident("user name")
		if p.acceptKw("identified") {
			p.expectKw("by")
			s.Password = p.stringLit("password")
		}
		return s
	}
	p.failNear("expected TABLE, INDEX, VIEW, PROCEDURE, FUNCTION, TRIGGER or USER")
	panic("unreachable")
}

func (p *Parser) createTable() Stmt {
	s := Stmt{Kind: SCreateTable}
	if p.acceptKw("if") {
		p.expectKw("not")
		p.expectKw("exists")
		s.IfNotExists = true
	}
	s.Table = p.ident("table name")
	p.expectSym("(")
	for {
		switch {
		case p.kw("primary"):
			p.advance()
			p.expectKw("key")
			s.PkCols = p.identList()
		case p.kw("unique"):
			p.advance()
			s.Uniques = append(s.Uniques, p.identList())
		default:
			c := ColDef{Name: p.ident("column name")}
			c.Type = p.parseType()
		colAttrLoop:
			for {
				switch {
				case p.acceptKw("not"):
					p.expectKw("null")
					c.NotNull = true
				case p.acceptKw("null"):
				case p.acceptKw("primary"):
					p.expectKw("key")
					c.PK = true
				case p.acceptKw("unique"):
					c.Unique = true
				case p.acceptKw("default"):
					c.Default = p.parseUnary()
				default:
					break colAttrLoop
				}
			}
			s.ColDefs = append(s.ColDefs, c)
		}
		if !p.acceptSym(",") {
			break
		}
	}
	p.expectSym(")")
	return s
}

func (p *Parser) createProcOrFunc(isFunc, replace bool) Stmt {
	kind := SCreateProc
	if isFunc {
		kind = SCreateFunc
	}
	s := Stmt{Kind: kind, IfExists: replace}
	s.Name = p.ident("name")
	p.expectSym("(")
	if !p.sym(")") {
		for {
			pr := Param{}
			if p.cur().Kind == TkVar {
				pr.Name = p.cur().Text
				p.advance()
			} else {
				pr.Name = "@" + p.ident("parameter name")
			}
			pr.Type = p.parseType()
			s.Params = append(s.Params, pr)
			if !p.acceptSym(",") {
				break
			}
		}
	}
	p.expectSym(")")
	if isFunc {
		p.expectKw("returns")
		s.ReturnType = p.parseType()
	}
	p.expectKw("begin")
	start := p.cur().Pos
	s.Body = p.stmtBlockUntilEnd()
	s.RawText = p.rawSlice(start)
	p.expectKw("end")
	return s
}

func (p *Parser) createTrigger() Stmt {
	s := Stmt{Kind: SCreateTrigger}
	s.Name = p.ident("trigger name")
	switch {
	case p.acceptKw("before"):
		s.TrigTiming = "BEFORE"
	case p.acceptKw("after"):
		s.TrigTiming = "AFTER"
	default:
		p.failNear("expected BEFORE or AFTER")
	}
	switch {
	case p.acceptKw("insert"):
		s.TrigEvent = "INSERT"
	case p.acceptKw("update"):
		s.TrigEvent = "UPDATE"
	case p.acceptKw("delete"):
		s.TrigEvent = "DELETE"
	default:
		p.failNear("expected INSERT, UPDATE or DELETE")
	}
	p.expectKw("on")
	s.Table = p.ident("table name")
	if p.acceptKw("for") {
		p.expectKw("each")
		p.expectKw("row")
	}
	p.expectKw("begin")
	start := p.cur().Pos
	s.Body = p.stmtBlockUntilEnd()
	s.RawText = p.rawSlice(start)
	p.expectKw("end")
	return s
}

// ---------------------------------------------------------------- DROP
func (p *Parser) drop() Stmt {
	var s Stmt
	switch {
	case p.acceptKw("table"):
		s.Kind = SDropTable
	case p.acceptKw("index"):
		s.Kind = SDropIndex
	case p.acceptKw("view"):
		s.Kind = SDropView
	case p.acceptKw("procedure"):
		s.Kind = SDropProc
	case p.acceptKw("function"):
		s.Kind = SDropFunc
	case p.acceptKw("trigger"):
		s.Kind = SDropTrigger
	case p.acceptKw("user"):
		s.Kind = SDropUser
	default:
		p.failNear("expected TABLE, INDEX, VIEW, PROCEDURE, FUNCTION, TRIGGER or USER")
	}
	if p.acceptKw("if") {
		p.expectKw("exists")
		s.IfExists = true
	}
	name := p.ident("name")
	if s.Kind == SDropTable {
		s.Table = name
	} else {
		s.Name = name
	}
	return s
}

func (p *Parser) alter() Stmt {
	s := Stmt{Kind: SAlterTable}
	p.expectKw("table")
	s.Table = p.ident("table name")
	p.expectKw("add")
	p.acceptKw("column")
	cd := &ColDef{Name: p.ident("column name")}
	cd.Type = p.parseType()
	for {
		switch {
		case p.acceptKw("not"):
			p.expectKw("null")
			cd.NotNull = true
		case p.acceptKw("null"):
		case p.acceptKw("default"):
			cd.Default = p.parseUnary()
		case p.acceptKw("unique"):
			cd.Unique = true
		default:
			return func() Stmt { s.AlterAdd = cd; return s }()
		}
	}
}

// --------------------------------------------------------------- INSERT
func (p *Parser) insert() Stmt {
	s := Stmt{Kind: SInsert}
	p.expectKw("into")
	s.Table = p.ident("table name")
	if p.sym("(") {
		s.InsertCols = p.identList()
	}
	if p.kw("select") {
		sel := p.selectWithSetOps()
		s.InsertSel = &sel
		return s
	}
	p.expectKw("values")
	for {
		p.expectSym("(")
		var row []*Expr
		for {
			row = append(row, p.expr())
			if !p.acceptSym(",") {
				break
			}
		}
		p.expectSym(")")
		s.Rows = append(s.Rows, row)
		if !p.acceptSym(",") {
			break
		}
	}
	return s
}

func (p *Parser) update() Stmt {
	s := Stmt{Kind: SUpdate}
	s.Table = p.ident("table name")
	p.expectKw("set")
	for {
		col := p.ident("column name")
		if !p.acceptSym("=") {
			p.expectSym(":=")
		}
		s.Sets = append(s.Sets, SetClause{Col: col, Value: p.expr()})
		if !p.acceptSym(",") {
			break
		}
	}
	s.Where = p.condClauses()
	return s
}

func (p *Parser) del() Stmt {
	s := Stmt{Kind: SDelete}
	p.expectKw("from")
	s.Table = p.ident("table name")
	s.Where = p.condClauses()
	return s
}

func (p *Parser) grant() Stmt {
	s := Stmt{Kind: SGrant}
	s.Privs = p.privList()
	p.expectKw("on")
	s.OnTable = p.tableOrStar()
	p.expectKw("to")
	s.ToUser = p.ident("user name")
	return s
}

func (p *Parser) revoke() Stmt {
	s := Stmt{Kind: SRevoke}
	s.Privs = p.privList()
	p.expectKw("on")
	s.OnTable = p.tableOrStar()
	p.expectKw("from")
	s.ToUser = p.ident("user name")
	return s
}

func (p *Parser) tableOrStar() string {
	if p.acceptSym("*") {
		return "*"
	}
	return p.ident("table name")
}

func (p *Parser) privList() []string {
	var out []string
	for {
		switch {
		case p.acceptKw("all"):
			p.acceptKw("privileges")
			out = append(out, "ALL")
		case p.acceptKw("select"):
			out = append(out, "SELECT")
		case p.acceptKw("insert"):
			out = append(out, "INSERT")
		case p.acceptKw("update"):
			out = append(out, "UPDATE")
		case p.acceptKw("delete"):
			out = append(out, "DELETE")
		default:
			p.failNear("expected a privilege (SELECT/INSERT/UPDATE/DELETE/ALL)")
		}
		if !p.acceptSym(",") {
			break
		}
	}
	return out
}

// ----------------------------------------------------- procedural blocks
func (p *Parser) stmtBlockUntilEnd() []PStmt {
	var out []PStmt
	for !p.kw("end") && !p.kw("else") && !p.kw("elsif") && p.cur().Kind != TkEnd {
		out = append(out, p.pstmt())
		p.acceptSym(";")
	}
	return out
}

func (p *Parser) pstmt() PStmt {
	switch {
	case p.acceptKw("declare"):
		ps := PStmt{Kind: PDeclare}
		if p.cur().Kind != TkVar {
			p.failNear("expected a @variable name")
		}
		ps.VarName = p.cur().Text
		p.advance()
		ps.VarType = p.parseType()
		if p.acceptKw("default") || p.acceptSym(":=") {
			ps.Default = p.expr()
		}
		return ps
	case p.acceptKw("set"):
		return p.pSetStmt()
	case p.acceptKw("if"):
		return p.pIf()
	case p.acceptKw("while"):
		ps := PStmt{Kind: PWhile}
		ps.Cond = p.expr()
		p.expectKw("do")
		ps.Body = p.stmtBlockUntilEnd()
		p.expectKw("end")
		p.acceptKw("while")
		return ps
	case p.acceptKw("for"):
		ps := PStmt{Kind: PForRange}
		if p.cur().Kind != TkVar {
			p.failNear("expected a @variable name")
		}
		ps.VarName = p.cur().Text
		p.advance()
		p.expectKw("in")
		ps.LoVal = p.expr()
		p.expectSym(".")
		p.expectSym(".")
		ps.HiVal = p.expr()
		p.expectKw("do")
		ps.Body = p.stmtBlockUntilEnd()
		p.expectKw("end")
		p.acceptKw("for")
		return ps
	case p.acceptKw("return"):
		ps := PStmt{Kind: PReturn}
		if !p.sym(";") && !p.kw("end") {
			ps.Value = p.expr()
		}
		return ps
	case p.acceptKw("call"):
		ps := PStmt{Kind: PCall}
		ps.CallName = p.ident("procedure name")
		p.expectSym("(")
		if !p.sym(")") {
			for {
				ps.CallArgs = append(ps.CallArgs, p.expr())
				if !p.acceptSym(",") {
					break
				}
			}
		}
		p.expectSym(")")
		return ps
	case p.acceptKw("raise"):
		p.acceptKw("notice")
		p.acceptKw("exception")
		ps := PStmt{Kind: PRaise}
		ps.Value = p.expr()
		return ps
	case p.acceptKw("begin"):
		ps := PStmt{Kind: PBlock}
		ps.Body = p.stmtBlockUntilEnd()
		p.expectKw("end")
		return ps
	default:
		// a plain SQL statement (INSERT/UPDATE/DELETE/SELECT) inside the body
		st := p.statement()
		return PStmt{Kind: PSql, SQL: &st}
	}
}

func (p *Parser) pSetStmt() PStmt {
	ps := PStmt{Kind: PSet}
	switch {
	case p.cur().Kind == TkVar:
		ps.Target = p.cur().Text
		p.advance()
	case (p.kw("new") || p.kw("old")) && p.peek(1).Kind == TkSymbol && p.peek(1).Text == ".":
		tbl := p.ident("NEW/OLD")
		p.expectSym(".")
		col := p.ident("column name")
		ps.Target = tbl + "." + col
	default:
		p.failNear("expected a @variable or NEW.column")
	}
	if !p.acceptSym("=") {
		p.expectSym(":=")
	}
	ps.Value = p.expr()
	return ps
}

func (p *Parser) pIf() PStmt {
	ps := PStmt{Kind: PIf}
	ps.Cond = p.expr()
	p.expectKw("then")
	ps.Body = p.stmtBlockUntilEnd()
	for p.acceptKw("elsif") {
		ei := ElseIf{Cond: p.expr()}
		p.expectKw("then")
		ei.Body = p.stmtBlockUntilEnd()
		ps.ElseIfs = append(ps.ElseIfs, ei)
	}
	if p.acceptKw("else") {
		ps.Else = p.stmtBlockUntilEnd()
	}
	p.expectKw("end")
	p.acceptKw("if")
	return ps
}

// -------------------------------------------------------------- SELECT
func (p *Parser) selectItem() SelectItem {
	var it SelectItem
	if p.acceptSym("*") {
		it.Star = true
		return it
	}
	if p.cur().Kind == TkIdent && p.peek(1).Kind == TkSymbol && p.peek(1).Text == "." &&
		p.peek(2).Kind == TkSymbol && p.peek(2).Text == "*" {
		it.Star = true
		it.StarTable = p.cur().Text
		p.advance()
		p.advance()
		p.advance()
		return it
	}
	it.Expr = p.expr()
	if p.acceptKw("as") {
		it.Alias = p.ident("alias")
	} else if p.cur().Kind == TkIdent && !reservedWords[p.cur().Text] {
		it.Alias = p.ident("alias")
	}
	return it
}

func (p *Parser) tableRef() TableRef {
	var r TableRef
	if p.acceptSym("(") {
		sel := p.selectWithSetOps()
		p.expectSym(")")
		r.Sub = &sel
		if p.acceptKw("as") {
			r.Alias = p.ident("alias")
		} else if p.cur().Kind == TkIdent && !reservedWords[p.cur().Text] {
			r.Alias = p.ident("alias")
		} else {
			fail("a derived table (subquery in FROM) needs an alias")
		}
		return r
	}
	r.Name = p.ident("table name")
	if p.acceptKw("as") {
		r.Alias = p.ident("alias")
	} else if p.cur().Kind == TkIdent && !reservedWords[p.cur().Text] {
		r.Alias = p.ident("alias")
	}
	return r
}

// selectCore parses one SELECT...LIMIT core without set-operator chaining.
func (p *Parser) selectCore() SelectStmt {
	p.expectKw("select")
	var q SelectStmt
	q.Limit = -1
	q.Distinct = p.acceptKw("distinct")
	p.acceptKw("all")
	for {
		q.Items = append(q.Items, p.selectItem())
		if !p.acceptSym(",") {
			break
		}
	}
	if p.acceptKw("into") {
		for {
			if p.cur().Kind != TkVar {
				p.failNear("expected a @variable in INTO list")
			}
			q.IntoVars = append(q.IntoVars, p.cur().Text)
			p.advance()
			if !p.acceptSym(",") {
				break
			}
		}
	}
	if p.acceptKw("from") {
		q.HasFrom = true
		q.From = p.tableRef()
	joinLoop:
		for {
			var j JoinClause
			cross := false
			switch {
			case p.acceptSym(","):
				cross = true
			case p.acceptKw("left"):
				p.acceptKw("outer")
				p.expectKw("join")
				j.Left = true
			case p.acceptKw("right"):
				p.acceptKw("outer")
				p.expectKw("join")
				j.Right = true
			case p.acceptKw("full"):
				p.acceptKw("outer")
				p.expectKw("join")
				j.Full = true
			case p.acceptKw("inner"):
				p.expectKw("join")
			case p.acceptKw("cross"):
				p.expectKw("join")
				cross = true
			case p.acceptKw("join"):
			default:
				break joinLoop
			}
			j.Ref = p.tableRef()
			if cross {
				j.On = litExpr(VBoolVal(true))
			} else {
				p.expectKw("on")
				j.On = p.expr()
			}
			q.Joins = append(q.Joins, j)
		}
	}
	q.Where = p.condClauses()
	if p.acceptKw("group") {
		p.expectKw("by")
		for {
			q.GroupBy = append(q.GroupBy, p.expr())
			if !p.acceptSym(",") {
				break
			}
		}
	}
	if p.acceptKw("having") {
		q.Having = p.expr()
	}
	if p.acceptKw("order") {
		p.expectKw("by")
		for {
			var o OrderItem
			o.Expr = p.expr()
			if p.acceptKw("desc") {
				o.Desc = true
			} else {
				p.acceptKw("asc")
			}
			q.OrderBy = append(q.OrderBy, o)
			if !p.acceptSym(",") {
				break
			}
		}
	}
	if p.acceptKw("limit") {
		q.Limit = p.intLit()
		if p.acceptSym(",") {
			// MySQL LIMIT offset, count
			off := q.Limit
			q.Limit = p.intLit()
			q.Offset = off
		} else if p.acceptKw("offset") {
			q.Offset = p.intLit()
		}
	}
	return q
}

// selectWithSetOps parses a chain of SELECTs joined by UNION/INTERSECT/EXCEPT.
func (p *Parser) selectWithSetOps() SelectStmt {
	first := p.selectCore()
	head := &first
	for {
		var op SetOp
		switch {
		case p.acceptKw("union"):
			if p.acceptKw("all") {
				op = SetOpUnionAll
			} else {
				p.acceptKw("distinct")
				op = SetOpUnion
			}
		case p.acceptKw("intersect"):
			op = SetOpIntersect
		case p.acceptKw("except"):
			op = SetOpExcept
		default:
			return first
		}
		next := p.selectCore()
		head.SetOp = op
		head.Combined = &next
		head = &next
	}
}

// ------------------------------------------------------- expressions
func binExpr(op string, a, b *Expr) *Expr { return &Expr{Kind: EKBinary, Op: op, Args: []*Expr{a, b}} }

func (p *Parser) expr() *Expr { return p.parseOr() }

func (p *Parser) parseOr() *Expr {
	l := p.parseAnd()
	for p.acceptKw("or") {
		l = binExpr("OR", l, p.parseAnd())
	}
	return l
}
func (p *Parser) parseAnd() *Expr {
	l := p.parseNot()
	for p.acceptKw("and") {
		l = binExpr("AND", l, p.parseNot())
	}
	return l
}
func (p *Parser) parseNot() *Expr {
	if p.acceptKw("not") {
		return &Expr{Kind: EKUnary, Op: "NOT", Args: []*Expr{p.parseNot()}}
	}
	return p.parseCmp()
}

func (p *Parser) parseCmp() *Expr {
	l := p.parseAdd()
	for {
		if p.cur().Kind == TkSymbol {
			s := p.cur().Text
			if s == "=" || s == "!=" || s == "<>" || s == "<" || s == "<=" || s == ">" || s == ">=" {
				op := s
				if s == "<>" {
					op = "!="
				}
				p.advance()
				l = binExpr(op, l, p.parseAdd())
				continue
			}
			return l
		}
		if p.kw("is") {
			p.advance()
			e := &Expr{Kind: EKIsNull}
			e.Neg = p.acceptKw("not")
			p.expectKw("null")
			e.Args = []*Expr{l}
			l = e
			continue
		}
		neg := false
		if p.kw("not") && p.peek(1).Kind == TkIdent &&
			(p.peek(1).Text == "like" || p.peek(1).Text == "in" || p.peek(1).Text == "between") {
			p.advance()
			neg = true
		}
		switch {
		case p.acceptKw("like"):
			e := &Expr{Kind: EKLike, Neg: neg, Args: []*Expr{l, p.parseAdd()}}
			l = e
		case p.acceptKw("in"):
			p.expectSym("(")
			if p.kw("select") {
				sub := p.selectWithSetOps()
				p.expectSym(")")
				l = &Expr{Kind: EKInSub, Neg: neg, Args: []*Expr{l}, Sub: &sub}
			} else {
				e := &Expr{Kind: EKIn, Neg: neg, Args: []*Expr{l}}
				for {
					e.Args = append(e.Args, p.expr())
					if !p.acceptSym(",") {
						break
					}
				}
				p.expectSym(")")
				l = e
			}
		case p.acceptKw("between"):
			lo := p.parseAdd()
			p.expectKw("and")
			hi := p.parseAdd()
			l = &Expr{Kind: EKBetween, Neg: neg, Args: []*Expr{l, lo, hi}}
		default:
			return l
		}
	}
}

func (p *Parser) parseAdd() *Expr {
	l := p.parseMul()
	for p.sym("+") || p.sym("-") || p.sym("||") {
		op := p.cur().Text
		p.advance()
		l = binExpr(op, l, p.parseMul())
	}
	return l
}
func (p *Parser) parseMul() *Expr {
	l := p.parseUnary()
	for p.sym("*") || p.sym("/") || p.sym("%") {
		op := p.cur().Text
		p.advance()
		l = binExpr(op, l, p.parseUnary())
	}
	return l
}
func (p *Parser) parseUnary() *Expr {
	if p.acceptSym("-") {
		a := p.parseUnary()
		if a.Kind == EKLit && a.Lit.Kind == VInt {
			return litExpr(VIntVal(-a.Lit.I))
		}
		if a.Kind == EKLit && a.Lit.Kind == VReal {
			return litExpr(VRealVal(-a.Lit.D))
		}
		return &Expr{Kind: EKUnary, Op: "-", Args: []*Expr{a}}
	}
	if p.acceptSym("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *Parser) parsePrimary() *Expr {
	t := p.cur()
	switch t.Kind {
	case TkNumber:
		p.advance()
		if t.IsReal {
			f, _ := strconv.ParseFloat(t.Text, 64)
			return litExpr(VRealVal(f))
		}
		iv, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			f, _ := strconv.ParseFloat(t.Text, 64)
			return litExpr(VRealVal(f))
		}
		return litExpr(VIntVal(iv))
	case TkString:
		p.advance()
		return litExpr(VTextVal(t.Text))
	case TkVar:
		p.advance()
		return &Expr{Kind: EKVar, Name: t.Text}
	case TkParam:
		p.advance()
		return &Expr{Kind: EKParam}
	}
	if p.acceptSym("(") {
		if p.kw("select") {
			sub := p.selectWithSetOps()
			p.expectSym(")")
			return &Expr{Kind: EKScalarSub, Sub: &sub}
		}
		e := p.expr()
		p.expectSym(")")
		return e
	}
	if t.Kind == TkIdent {
		switch t.Text {
		case "null":
			p.advance()
			return litExpr(VNullVal())
		case "true":
			p.advance()
			return litExpr(VBoolVal(true))
		case "false":
			p.advance()
			return litExpr(VBoolVal(false))
		case "exists":
			p.advance()
			p.expectSym("(")
			sub := p.selectWithSetOps()
			p.expectSym(")")
			return &Expr{Kind: EKExistsSub, Sub: &sub}
		case "case":
			return p.parseCase()
		case "cast":
			return p.parseCast()
		case "new", "old":
			// NEW.col / OLD.col inside trigger bodies: ordinary qualified column
		}
		if reservedWords[t.Text] {
			p.failNear("unexpected keyword")
		}
		p.advance()
		if p.acceptSym("(") { // function call
			e := &Expr{Kind: EKFunc, Name: t.Text}
			e.IsAgg = t.Text == "count" || t.Text == "sum" || t.Text == "avg" || t.Text == "min" || t.Text == "max"
			if p.acceptSym("*") {
				e.Args = append(e.Args, &Expr{Kind: EKStar})
			} else if !p.sym(")") {
				if e.IsAgg && p.acceptKw("distinct") {
					e.Distinct = true
				}
				for {
					e.Args = append(e.Args, p.expr())
					if !p.acceptSym(",") {
						break
					}
				}
			}
			p.expectSym(")")
			return e
		}
		e := &Expr{Kind: EKCol}
		if p.acceptSym(".") {
			e.Table = t.Text
			e.Name = p.ident("column name")
		} else {
			e.Name = t.Text
		}
		return e
	}
	p.failNear("expected expression")
	panic("unreachable")
}

func (p *Parser) parseCase() *Expr {
	p.expectKw("case")
	e := &Expr{Kind: EKCase}
	if !p.kw("when") {
		e.CaseOperand = p.expr()
	}
	for p.acceptKw("when") {
		cond := p.expr()
		p.expectKw("then")
		val := p.expr()
		e.WhenConds = append(e.WhenConds, cond)
		e.WhenVals = append(e.WhenVals, val)
	}
	if len(e.WhenConds) == 0 {
		p.failNear("expected WHEN")
	}
	if p.acceptKw("else") {
		e.ElseVal = p.expr()
	}
	p.expectKw("end")
	return e
}

func (p *Parser) parseCast() *Expr {
	p.expectKw("cast")
	p.expectSym("(")
	e := &Expr{Kind: EKCast}
	e.Args = []*Expr{p.expr()}
	p.expectKw("as")
	e.CastType = p.parseType()
	p.expectSym(")")
	return e
}

// exprToString renders an expression back to SQL text, used for default
// result-column labels and EXPLAIN output.
func exprToString(e *Expr) string {
	join := func(from int) string {
		var sb strings.Builder
		for i := from; i < len(e.Args); i++ {
			if i > from {
				sb.WriteString(", ")
			}
			sb.WriteString(exprToString(e.Args[i]))
		}
		return sb.String()
	}
	switch e.Kind {
	case EKLit:
		if e.Lit.Kind == VText {
			return "'" + e.Lit.S + "'"
		}
		return e.Lit.String()
	case EKCol:
		if e.Table == "" {
			return e.Name
		}
		return e.Table + "." + e.Name
	case EKVar:
		return e.Name
	case EKParam:
		return "?"
	case EKStar:
		return "*"
	case EKUnary:
		if e.Op == "NOT" {
			return "NOT " + exprToString(e.Args[0])
		}
		return "-" + exprToString(e.Args[0])
	case EKBinary:
		return exprToString(e.Args[0]) + " " + e.Op + " " + exprToString(e.Args[1])
	case EKFunc:
		d := ""
		if e.Distinct {
			d = "DISTINCT "
		}
		return e.Name + "(" + d + join(0) + ")"
	case EKIsNull:
		if e.Neg {
			return exprToString(e.Args[0]) + " IS NOT NULL"
		}
		return exprToString(e.Args[0]) + " IS NULL"
	case EKIn:
		if e.Neg {
			return exprToString(e.Args[0]) + " NOT IN (" + join(1) + ")"
		}
		return exprToString(e.Args[0]) + " IN (" + join(1) + ")"
	case EKInSub:
		if e.Neg {
			return exprToString(e.Args[0]) + " NOT IN (subquery)"
		}
		return exprToString(e.Args[0]) + " IN (subquery)"
	case EKExistsSub:
		return "EXISTS (subquery)"
	case EKScalarSub:
		return "(subquery)"
	case EKBetween:
		if e.Neg {
			return exprToString(e.Args[0]) + " NOT BETWEEN " + exprToString(e.Args[1]) + " AND " + exprToString(e.Args[2])
		}
		return exprToString(e.Args[0]) + " BETWEEN " + exprToString(e.Args[1]) + " AND " + exprToString(e.Args[2])
	case EKLike:
		if e.Neg {
			return exprToString(e.Args[0]) + " NOT LIKE " + exprToString(e.Args[1])
		}
		return exprToString(e.Args[0]) + " LIKE " + exprToString(e.Args[1])
	case EKCase:
		return "CASE"
	case EKCast:
		return "CAST(" + exprToString(e.Args[0]) + ")"
	}
	return "?"
}

// condClauses parses the book's condition syntax (ch.13) next to plain SQL:
//
//	INDEX BY <indexing cond> FILTER <cond>   |   WHERE <cond>
//
// INDEX BY and FILTER are both just AND-ed into the predicate; the planner
// then picks the index from the leading conjuncts, as in the book.
func (p *Parser) condClauses() *Expr {
	var out *Expr
	add := func(e *Expr) {
		if out == nil {
			out = e
		} else {
			out = binExpr("AND", out, e)
		}
	}
	if p.acceptKw("index") {
		p.expectKw("by")
		add(p.expr())
	}
	if p.acceptKw("filter") {
		add(p.expr())
	}
	if p.acceptKw("where") {
		add(p.expr())
	}
	return out
}
