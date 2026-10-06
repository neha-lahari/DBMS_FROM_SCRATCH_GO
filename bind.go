package main

type SchemaCol struct{ Table, Name string }
type Schema []SchemaCol

// bindExpr resolves every Col in e against schema (depth 0), then each
// outer schema in order (depth 1, 2, ...) for correlated subqueries.
func bindExpr(e *Expr, schema Schema, outer []Schema) {
	switch e.Kind {
	case EKCol:
		if e.Table == "new" || e.Table == "old" {
			return
		}
		if idx, ok := findCol(schema, e.Table, e.Name); ok {
			e.Idx, e.OuterDepth = idx, 0
			return
		}
		for depth, sc := range outer {
			if idx, ok := findCol(sc, e.Table, e.Name); ok {
				e.Idx, e.OuterDepth = idx, depth+1
				return
			}
		}
		fail("no such column: %s", exprToString(e))
	case EKInSub, EKExistsSub, EKScalarSub:
		// The subquery binds its own FROM-clause columns itself (see
		// executor.go); here we only need to bind any correlated
		// references in e.Args (e.g. the LHS of `x IN (subquery)`).
		for _, a := range e.Args {
			bindExpr(a, schema, outer)
		}
	default:
		for _, a := range e.Args {
			bindExpr(a, schema, outer)
		}
		if e.CaseOperand != nil {
			bindExpr(e.CaseOperand, schema, outer)
		}
		for _, w := range e.WhenConds {
			bindExpr(w, schema, outer)
		}
		for _, w := range e.WhenVals {
			bindExpr(w, schema, outer)
		}
		if e.ElseVal != nil {
			bindExpr(e.ElseVal, schema, outer)
		}
	}
}

func findCol(schema Schema, table, name string) (int, bool) {
	found := -1
	for i, sc := range schema {
		if sc.Name != name {
			continue
		}
		if table != "" && sc.Table != table {
			continue
		}
		if found >= 0 {
			fail("ambiguous column name: %s", name)
		}
		found = i
	}
	if found < 0 {
		return 0, false
	}
	return found, true
}

func containsAgg(e *Expr) bool {
	if e == nil {
		return false
	}
	if e.Kind == EKFunc && e.IsAgg {
		return true
	}
	if e.Kind == EKInSub || e.Kind == EKExistsSub || e.Kind == EKScalarSub {
		return false // aggregates inside a subquery belong to that subquery, not this one
	}
	for _, a := range e.Args {
		if containsAgg(a) {
			return true
		}
	}
	if containsAgg(e.CaseOperand) {
		return true
	}
	for _, w := range e.WhenConds {
		if containsAgg(w) {
			return true
		}
	}
	for _, w := range e.WhenVals {
		if containsAgg(w) {
			return true
		}
	}
	return containsAgg(e.ElseVal)
}

func collectAggs(e *Expr, out *[]*Expr, seen map[*Expr]bool) {
	if e == nil {
		return
	}
	if e.Kind == EKFunc && e.IsAgg {
		if seen[e] {
			return
		}
		seen[e] = true
		if len(e.Args) != 1 {
			fail("%s() takes exactly one argument", e.Name)
		}
		if e.Args[0].Kind == EKStar && e.Name != "count" {
			fail("%s(*) is not valid", e.Name)
		}
		if containsAgg(e.Args[0]) {
			fail("nested aggregate functions are not allowed")
		}
		e.Idx = len(*out)
		*out = append(*out, e)
		return
	}
	if e.Kind == EKInSub || e.Kind == EKExistsSub || e.Kind == EKScalarSub {
		return
	}
	for _, a := range e.Args {
		collectAggs(a, out, seen)
	}
	collectAggs(e.CaseOperand, out, seen)
	for _, w := range e.WhenConds {
		collectAggs(w, out, seen)
	}
	for _, w := range e.WhenVals {
		collectAggs(w, out, seen)
	}
	collectAggs(e.ElseVal, out, seen)
}
