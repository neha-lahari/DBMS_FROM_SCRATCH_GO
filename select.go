
package main

import (
	"sort"
	"strconv"
	"strings"
)

type subqCtx struct {
	schema Schema
	outer  []Schema
}

// subEval is installed as EvalCtx.SubEval and runs a correlated or
// non-correlated subquery expression, returning its scalar/boolean result.
func (ex *Executor) subEval(e *Expr, ctx *EvalCtx) Value {
	if len(ex.subqStack) == 0 {
		fail("internal error: subquery evaluated with no enclosing query context")
	}
	top := ex.subqStack[len(ex.subqStack)-1]
	newOuter := append([]Schema{top.schema}, top.outer...)
	switch e.Kind {
	case EKScalarSub:
		res := ex.Select(*e.Sub, newOuter, ctx)
		if len(res.Rows) == 0 {
			return VNullVal()
		}
		if len(res.Rows) > 1 {
			fail("scalar subquery returned more than one row")
		}
		if len(res.Rows[0]) != 1 {
			fail("scalar subquery must return exactly one column")
		}
		return res.Rows[0][0]
	case EKExistsSub:
		res := ex.Select(*e.Sub, newOuter, ctx)
		return VBoolVal(len(res.Rows) > 0)
	case EKInSub:
		lhs := evalExpr(e.Args[0], ctx)
		if lhs.IsNull() {
			return VNullVal()
		}
		res := ex.Select(*e.Sub, newOuter, ctx)
		sawNull := false
		for _, r := range res.Rows {
			if len(r) != 1 {
				fail("subquery in IN must return exactly one column")
			}
			if r[0].IsNull() {
				sawNull = true
				continue
			}
			if cmpNonNull(lhs, r[0]) == 0 {
				return VBoolVal(!e.Neg)
			}
		}
		if sawNull {
			return VNullVal()
		}
		return VBoolVal(e.Neg)
	}
	fail("internal error: not a subquery expression")
	return VNullVal()
}

// srcInfo is one resolved FROM/JOIN source: either a real table (planner
// pushdown possible) or a materialised row set (view, derived table, or a
// joined-in table that's been fully scanned).
type srcInfo struct {
	alias  string
	schema Schema
	table  *TableDef // nil for a view or derived table
	rows   []Row     // populated for non-table sources
}

func (ex *Executor) resolveTableRef(ref TableRef, outerSchemas []Schema, outerCtx *EvalCtx) srcInfo {
	if ref.Sub != nil {
		res := ex.Select(*ref.Sub, outerSchemas, outerCtx)
		alias := ref.Alias
		sc := make(Schema, len(res.Columns))
		for i, cn := range res.Columns {
			sc[i] = SchemaCol{Table: alias, Name: cn}
		}
		return srcInfo{alias: alias, schema: sc, rows: res.Rows}
	}
	if v, ok := ex.cat.Views[ref.Name]; ok {
		alias := ref.Alias
		if alias == "" {
			alias = ref.Name
		}
		vq := ParseSelectText(v.Query)
		res := ex.Select(vq, outerSchemas, outerCtx)
		sc := make(Schema, len(res.Columns))
		for i, cn := range res.Columns {
			sc[i] = SchemaCol{Table: alias, Name: cn}
		}
		return srcInfo{alias: alias, schema: sc, rows: res.Rows}
	}
	t, ok := ex.cat.Tables[ref.Name]
	if !ok {
		fail("no such table: %s", ref.Name)
	}
	ex.checkPriv(ref.Name, "SELECT")
	alias := ref.Alias
	if alias == "" {
		alias = ref.Name
	}
	sc := make(Schema, len(t.Cols))
	for i, c := range t.Cols {
		sc[i] = SchemaCol{Table: alias, Name: c.Name}
	}
	return srcInfo{alias: alias, schema: sc, table: t}
}

func (ex *Executor) materialize(s srcInfo) []Row {
	if s.table == nil {
		return s.rows
	}
	var rows []Row
	ex.scan(s.table, ex.plan(s.table, s.alias, nil), func(r StoredRow) { rows = append(rows, r.Vals) })
	return rows
}

// findEqui finds equality conjuncts "left.col = right.col" in a JOIN ... ON
// clause, returning (left index, right index-within-right-side) pairs.
func findEqui(on *Expr, rightOffset int) [][2]int {
	var conj []*Expr
	flattenAnd(on, &conj)
	var out [][2]int
	for _, c := range conj {
		if c.Kind != EKBinary || c.Op != "=" {
			continue
		}
		a, b := c.Args[0], c.Args[1]
		if a.Kind != EKCol || b.Kind != EKCol || a.OuterDepth != 0 || b.OuterDepth != 0 {
			continue
		}
		i, j := a.Idx, b.Idx
		if i < rightOffset && j >= rightOffset {
			out = append(out, [2]int{i, j - rightOffset})
		} else if j < rightOffset && i >= rightOffset {
			out = append(out, [2]int{j, i - rightOffset})
		}
	}
	return out
}

// Select runs a full SELECT, including any UNION/INTERSECT/EXCEPT chain.
// outerSchemas/outerCtx are non-nil when this is a correlated subquery.
func (ex *Executor) Select(q SelectStmt, outerSchemas []Schema, outerCtx *EvalCtx) Result {
	if q.SetOp == SetOpNone {
		return ex.execCore(&q, outerSchemas, outerCtx)
	}
	head := q
	head.SetOp, head.Combined = SetOpNone, nil
	head.OrderBy, head.Limit, head.Offset = nil, -1, 0
	left := ex.execCore(&head, outerSchemas, outerCtx)
	right := ex.Select(*q.Combined, outerSchemas, outerCtx)
	combined := combineSets(left, right, q.SetOp)
	applyFinalOrderLimit(&combined, q.OrderBy, q.Limit, q.Offset)
	return combined
}

func rowSetKey(r Row) string {
	var sb strings.Builder
	for _, v := range r {
		part := canon(v)
		sb.WriteString(strconv.Itoa(len(part)))
		sb.WriteByte(':')
		sb.WriteString(part)
	}
	return sb.String()
}

func combineSets(l, r Result, op SetOp) Result {
	out := Result{IsQuery: true, Columns: l.Columns}
	switch op {
	case SetOpUnionAll:
		out.Rows = append(append([]Row{}, l.Rows...), r.Rows...)
	case SetOpUnion:
		seen := map[string]bool{}
		add := func(rows []Row) {
			for _, row := range rows {
				k := rowSetKey(row)
				if !seen[k] {
					seen[k] = true
					out.Rows = append(out.Rows, row)
				}
			}
		}
		add(l.Rows)
		add(r.Rows)
	case SetOpIntersect:
		rset := map[string]bool{}
		for _, row := range r.Rows {
			rset[rowSetKey(row)] = true
		}
		seen := map[string]bool{}
		for _, row := range l.Rows {
			k := rowSetKey(row)
			if rset[k] && !seen[k] {
				seen[k] = true
				out.Rows = append(out.Rows, row)
			}
		}
	case SetOpExcept:
		rset := map[string]bool{}
		for _, row := range r.Rows {
			rset[rowSetKey(row)] = true
		}
		seen := map[string]bool{}
		for _, row := range l.Rows {
			k := rowSetKey(row)
			if !rset[k] && !seen[k] {
				seen[k] = true
				out.Rows = append(out.Rows, row)
			}
		}
	}
	out.Affected = int64(len(out.Rows))
	return out
}

// applyFinalOrderLimit sorts/limits a combined set-operator result using
// only ordinal positions or output column names (the standard SQL rule for
// ORDER BY after UNION/INTERSECT/EXCEPT).
func applyFinalOrderLimit(res *Result, orderBy []OrderItem, limit, offset int64) {
	if len(orderBy) > 0 {
		idx := make([]int, len(orderBy))
		desc := make([]bool, len(orderBy))
		for i, o := range orderBy {
			desc[i] = o.Desc
			if o.Expr.Kind == EKLit && o.Expr.Lit.Kind == VInt {
				p := int(o.Expr.Lit.I)
				if p < 1 || p > len(res.Columns) {
					fail("ORDER BY position out of range")
				}
				idx[i] = p - 1
			} else if o.Expr.Kind == EKCol && o.Expr.Table == "" {
				found := -1
				for j, c := range res.Columns {
					if c == o.Expr.Name {
						found = j
						break
					}
				}
				if found < 0 {
					fail("ORDER BY: no such output column '%s' (after UNION/INTERSECT/EXCEPT, ORDER BY may only use output column names or positions)", o.Expr.Name)
				}
				idx[i] = found
			} else {
				fail("ORDER BY after UNION/INTERSECT/EXCEPT may only use output column names or positions")
			}
		}
		sort.SliceStable(res.Rows, func(a, b int) bool {
			for i, c := range idx {
				r := CompareValues(res.Rows[a][c], res.Rows[b][c])
				if r != 0 {
					if desc[i] {
						return r > 0
					}
					return r < 0
				}
			}
			return false
		})
	}
	start := int64(0)
	if offset > 0 {
		start = offset
	}
	if start > int64(len(res.Rows)) {
		start = int64(len(res.Rows))
	}
	end := int64(len(res.Rows))
	if limit >= 0 && start+limit < end {
		end = start + limit
	}
	res.Rows = res.Rows[start:end]
	res.Affected = int64(len(res.Rows))
}

type outItem struct {
	expr    *Expr
	label   string
	aliased bool
}

// execCore runs one SELECT...LIMIT core (no set-operator chaining).
func (ex *Executor) execCore(q *SelectStmt, outerSchemas []Schema, outerCtx *EvalCtx) Result {
	var srcs []srcInfo
	var schema Schema
	if q.HasFrom {
		base := ex.resolveTableRef(q.From, outerSchemas, outerCtx)
		srcs = append(srcs, base)
		schema = append(schema, base.schema...)
		for _, j := range q.Joins {
			s := ex.resolveTableRef(j.Ref, outerSchemas, outerCtx)
			srcs = append(srcs, s)
			schema = append(schema, s.schema...)
		}
		for _, j := range q.Joins {
			bindExpr(j.On, schema, outerSchemas)
			if containsAgg(j.On) {
				fail("aggregate functions are not allowed in ON")
			}
		}
	}

	// This query's own (schema, outerSchemas) becomes the "outer" context a
	// subquery nested inside it should see; push it for the duration of
	// planning + row evaluation below.
	ex.subqStack = append(ex.subqStack, subqCtx{schema: schema, outer: outerSchemas})
	defer func() { ex.subqStack = ex.subqStack[:len(ex.subqStack)-1] }()

	// ---- select list: expand * / t.*
	var outs []outItem
	for _, it := range q.Items {
		if it.Star {
			any := false
			for i, sc := range schema {
				if it.StarTable != "" && sc.Table != it.StarTable {
					continue
				}
				e := &Expr{Kind: EKCol, Table: sc.Table, Name: sc.Name, Idx: i}
				outs = append(outs, outItem{expr: e, label: sc.Name})
				any = true
			}
			if !any {
				if it.StarTable == "" {
					fail("SELECT * requires a FROM clause")
				}
				fail("no such table: %s", it.StarTable)
			}
		} else {
			bindExpr(it.Expr, schema, outerSchemas)
			label := it.Alias
			if label == "" {
				if it.Expr.Kind == EKCol {
					label = it.Expr.Name
				} else {
					label = exprToString(it.Expr)
				}
			}
			outs = append(outs, outItem{expr: it.Expr, label: label, aliased: it.Alias != ""})
		}
	}

	// ---- ORDER BY: ordinals and select-list aliases resolved first
	var orderExprs []*Expr
	var orderDesc []bool
	for _, o := range q.OrderBy {
		e := o.Expr
		if e.Kind == EKLit && e.Lit.Kind == VInt {
			if e.Lit.I < 1 || int(e.Lit.I) > len(outs) {
				fail("ORDER BY position out of range")
			}
			e = outs[e.Lit.I-1].expr
		} else {
			matched := false
			if e.Kind == EKCol && e.Table == "" {
				for _, o2 := range outs {
					if o2.aliased && o2.label == e.Name {
						e = o2.expr
						matched = true
						break
					}
				}
			}
			if !matched {
				bindExpr(e, schema, outerSchemas)
			}
		}
		orderExprs = append(orderExprs, e)
		orderDesc = append(orderDesc, o.Desc)
	}

	if q.Where != nil {
		bindExpr(q.Where, schema, outerSchemas)
		if containsAgg(q.Where) {
			fail("aggregate functions are not allowed in WHERE")
		}
	}
	for _, g := range q.GroupBy {
		bindExpr(g, schema, outerSchemas)
		if containsAgg(g) {
			fail("aggregate functions are not allowed in GROUP BY")
		}
	}
	if q.Having != nil {
		bindExpr(q.Having, schema, outerSchemas)
	}

	var aggs []*Expr
	seen := map[*Expr]bool{}
	for _, o := range outs {
		collectAggs(o.expr, &aggs, seen)
	}
	if q.Having != nil {
		collectAggs(q.Having, &aggs, seen)
	}
	for _, e := range orderExprs {
		collectAggs(e, &aggs, seen)
	}
	grouped := len(q.GroupBy) > 0 || len(aggs) > 0 || q.Having != nil

	// ---- FROM / JOIN: gather rows
	var rows []Row
	if !q.HasFrom {
		rows = []Row{{}}
	} else {
		base := srcs[0]
		noJoins := len(q.Joins) == 0
		if base.table != nil && noJoins {
			ap := ex.plan(base.table, base.alias, q.Where)
			ex.scan(base.table, ap, func(r StoredRow) {
				if q.Where != nil {
					ctx := ex.ctx(r.Vals, outerCtx)
					if truth(evalExpr(q.Where, ctx)) != 1 {
						return
					}
				}
				rows = append(rows, r.Vals)
			})
		} else {
			rows = append(rows, ex.materialize(base)...)
			for k := 1; k < len(srcs); k++ {
				j := q.Joins[k-1]
				rightOffset := 0
				for i := 0; i < k; i++ {
					rightOffset += len(srcs[i].schema)
				}
				rrows := ex.materialize(srcs[k])
				equi := findEqui(j.On, rightOffset)
				hash := map[string][]int{}
				if len(equi) > 0 {
					for i, rr := range rrows {
						v := rr[equi[0][1]]
						if !v.IsNull() {
							k2 := canon(v)
							hash[k2] = append(hash[k2], i)
						}
					}
				}
				var next []Row
				matchedRight := make([]bool, len(rrows))
				for _, l := range rows {
					matched := false
					tryRight := func(ri int, rr Row) {
						comb := append(append(Row{}, l...), rr...)
						ctx := ex.ctx(comb, outerCtx)
						if truth(evalExpr(j.On, ctx)) == 1 {
							next = append(next, comb)
							matched = true
							matchedRight[ri] = true
						}
					}
					if len(equi) > 0 {
						v := l[equi[0][0]]
						if !v.IsNull() {
							for _, ri := range hash[canon(v)] {
								tryRight(ri, rrows[ri])
							}
						}
					} else {
						for ri, rr := range rrows {
							tryRight(ri, rr)
						}
					}
					if !matched && (j.Left || j.Full) {
						comb := append(append(Row{}, l...), make(Row, len(srcs[k].schema))...)
						for i := len(l); i < len(comb); i++ {
							comb[i] = VNullVal()
						}
						next = append(next, comb)
					}
				}
				if j.Right || j.Full {
					for ri, rr := range rrows {
						if matchedRight[ri] {
							continue
						}
						comb := append(Row{}, make(Row, rightOffset)...)
						for i := range comb {
							comb[i] = VNullVal()
						}
						comb = append(comb, rr...)
						next = append(next, comb)
					}
				}
				rows = next
			}
			if q.Where != nil {
				var kept []Row
				for _, r := range rows {
					ctx := ex.ctx(r, outerCtx)
					if truth(evalExpr(q.Where, ctx)) == 1 {
						kept = append(kept, r)
					}
				}
				rows = kept
			}
		}
	}

	// ---- projection / grouping
	type outRow struct{ vals, keys Row }
	var result []outRow
	emit := func(rep Row, aggVals Row) {
		ctx := ex.ctxAgg(rep, aggVals, outerCtx)
		var o outRow
		for _, out := range outs {
			o.vals = append(o.vals, evalExpr(out.expr, ctx))
		}
		for _, e := range orderExprs {
			o.keys = append(o.keys, evalExpr(e, ctx))
		}
		result = append(result, o)
	}
	if !grouped {
		for _, r := range rows {
			emit(r, nil)
		}
	} else {
		type group struct {
			rep Row
			st  []*AggState
		}
		var groups []group
		gidx := map[string]int{}
		for _, r := range rows {
			var key strings.Builder
			ctx := ex.ctx(r, outerCtx)
			for _, g := range q.GroupBy {
				part := canon(evalExpr(g, ctx))
				key.WriteString(strconv.Itoa(len(part)))
				key.WriteByte(':')
				key.WriteString(part)
			}
			ks := key.String()
			gi, ok := gidx[ks]
			if !ok {
				gi = len(groups)
				gidx[ks] = gi
				st := make([]*AggState, len(aggs))
				for i := range st {
					st[i] = newAggState()
				}
				groups = append(groups, group{rep: r, st: st})
			}
			for a := range aggs {
				accumulate(groups[gi].st[a], aggs[a], ctx)
			}
		}
		if len(groups) == 0 && len(q.GroupBy) == 0 { // aggregate over zero rows still yields one row
			st := make([]*AggState, len(aggs))
			for i := range st {
				st[i] = newAggState()
			}
			groups = append(groups, group{rep: make(Row, len(schema)), st: st})
		}
		for _, g := range groups {
			av := make(Row, len(aggs))
			for a := range aggs {
				av[a] = finalizeAgg(g.st[a], aggs[a])
			}
			if q.Having != nil {
				ctx := ex.ctxAgg(g.rep, av, outerCtx)
				if truth(evalExpr(q.Having, ctx)) != 1 {
					continue
				}
			}
			emit(g.rep, av)
		}
	}

	if q.Distinct {
		seenRows := map[string]bool{}
		var uniq []outRow
		for _, o := range result {
			k := rowSetKey(o.vals)
			if !seenRows[k] {
				seenRows[k] = true
				uniq = append(uniq, o)
			}
		}
		result = uniq
	}
	if len(orderExprs) > 0 {
		sort.SliceStable(result, func(a, b int) bool {
			for i := range orderExprs {
				c := CompareValues(result[a].keys[i], result[b].keys[i])
				if c != 0 {
					if orderDesc[i] {
						return c > 0
					}
					return c < 0
				}
			}
			return false
		})
	}

	res := Result{IsQuery: true}
	for _, o := range outs {
		res.Columns = append(res.Columns, o.label)
	}
	start := int64(0)
	if q.Offset > 0 {
		start = q.Offset
	}
	if start > int64(len(result)) {
		start = int64(len(result))
	}
	end := int64(len(result))
	if q.Limit >= 0 && start+q.Limit < end {
		end = start + q.Limit
	}
	for i := start; i < end; i++ {
		res.Rows = append(res.Rows, result[i].vals)
	}
	res.Affected = int64(len(res.Rows))

	if len(q.IntoVars) > 0 {
		if len(res.Rows) == 0 {
			for _, vn := range q.IntoVars {
				ex.setIntoVar(vn, VNullVal())
			}
		} else {
			if len(res.Rows[0]) != len(q.IntoVars) {
				fail("SELECT INTO: %d columns selected but %d variables given", len(res.Rows[0]), len(q.IntoVars))
			}
			for i, vn := range q.IntoVars {
				ex.setIntoVar(vn, res.Rows[0][i])
			}
		}
	}
	return res
}

// explainPlan produces a short human-readable query plan description
// (without executing anything). It doesn't descend into subqueries/views.
func (ex *Executor) explainPlan(q *SelectStmt) []string {
	var lines []string
	if q.HasFrom {
		t, isTable := ex.cat.Tables[q.From.Name]
		if isTable {
			ap := ex.plan(t, firstNonEmpty(q.From.Alias, q.From.Name), q.Where)
			lines = append(lines, ap.Desc)
		} else {
			lines = append(lines, "SCAN "+firstNonEmpty(q.From.Alias, q.From.Name)+" (view or derived table)")
		}
		for _, j := range q.Joins {
			name := firstNonEmpty(j.Ref.Alias, j.Ref.Name)
			strat := "NESTED LOOP JOIN"
			if j.On != nil {
				var conj []*Expr
				flattenAnd(j.On, &conj)
				for _, c := range conj {
					if c.Kind == EKBinary && c.Op == "=" && c.Args[0].Kind == EKCol && c.Args[1].Kind == EKCol {
						strat = "HASH JOIN"
						break
					}
				}
			}
			prefix := ""
			if j.Left {
				prefix = "LEFT "
			}
			lines = append(lines, prefix+strat+" "+name)
		}
	} else {
		lines = append(lines, "CONSTANT ROW")
	}
	grouped := len(q.GroupBy) > 0 || q.Having != nil
	if !grouped {
		for _, it := range q.Items {
			if it.Expr != nil && it.Expr.Kind == EKFunc && it.Expr.IsAgg {
				grouped = true
			}
		}
	}
	if grouped {
		if len(q.GroupBy) > 0 {
			lines = append(lines, "GROUP BY (hash)")
		} else {
			lines = append(lines, "AGGREGATE")
		}
	}
	if q.Distinct {
		lines = append(lines, "DISTINCT")
	}
	if len(q.OrderBy) > 0 {
		lines = append(lines, "SORT")
	}
	if q.Limit >= 0 {
		l := "LIMIT " + itoa(q.Limit)
		if q.Offset != 0 {
			l += " OFFSET " + itoa(q.Offset)
		}
		lines = append(lines, l)
	}
	if q.SetOp != SetOpNone {
		names := map[SetOp]string{SetOpUnion: "UNION", SetOpUnionAll: "UNION ALL", SetOpIntersect: "INTERSECT", SetOpExcept: "EXCEPT"}
		lines = append(lines, names[q.SetOp])
		lines = append(lines, ex.explainPlan(q.Combined)...)
	}
	return lines
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
