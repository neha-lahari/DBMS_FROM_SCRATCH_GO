// eval.go - expression evaluation (scalar + aggregate), three-valued logic.
package main

import (
	"math"
	"strconv"
	"strings"
)


type EvalCtx struct {
	Row   []Value
	Aggs  []Value
	Vars  map[string]Value // procedure/function/trigger local variables, keyed by "@name"
	Outer *EvalCtx

	SubEval func(e *Expr, ctx *EvalCtx) Value

	CallFunc func(name string, args []Value) (Value, bool)
	TrigTable *TableDef
	TrigNew   Row
	TrigOld   Row
}

func (c *EvalCtx) outerN(n int) *EvalCtx {
	for i := 0; i < n; i++ {
		c = c.Outer
	}
	return c
}

func (c *EvalCtx) lookupVar(name string) (Value, bool) {
	for ctx := c; ctx != nil; ctx = ctx.Outer {
		if ctx.Vars != nil {
			if v, ok := ctx.Vars[name]; ok {
				return v, true
			}
		}
	}
	return Value{}, false
}

// truth implements three-valued logic: -1 unknown (NULL), 0 false, 1 true.
func truth(v Value) int {
	switch v.Kind {
	case VNull:
		return -1
	case VBool, VInt:
		if v.I != 0 {
			return 1
		}
		return 0
	case VReal:
		if v.D != 0 {
			return 1
		}
		return 0
	}
	fail("expected a boolean expression")
	return -1
}

func fromTruth(t int) Value {
	if t < 0 {
		return VNullVal()
	}
	return VBoolVal(t == 1)
}

func cmpNonNull(a, b Value) int {
	if (a.IsNumeric() && b.IsNumeric()) || a.Kind == b.Kind {
		return CompareValues(a, b)
	}
	fail("cannot compare values of different types")
	return 0
}

func arith(op string, a, b Value) Value {
	if a.IsNull() || b.IsNull() {
		return VNullVal()
	}
	if op == "||" {
		return VTextVal(a.String() + b.String())
	}
	if !a.IsNumeric() || !b.IsNumeric() {
		fail("arithmetic on a non-numeric value")
	}
	if a.Kind == VInt && b.Kind == VInt {
		x, y := a.I, b.I
		switch op {
		case "+":
			return VIntVal(int64(uint64(x) + uint64(y)))
		case "-":
			return VIntVal(int64(uint64(x) - uint64(y)))
		case "*":
			return VIntVal(int64(uint64(x) * uint64(y)))
		case "/":
			if y == 0 {
				fail("division by zero")
			}
			return VIntVal(x / y)
		case "%":
			if y == 0 {
				fail("division by zero")
			}
			return VIntVal(x % y)
		}
	}
	x, y := a.AsFloat(), b.AsFloat()
	switch op {
	case "+":
		return VRealVal(x + y)
	case "-":
		return VRealVal(x - y)
	case "*":
		return VRealVal(x * y)
	case "/":
		if y == 0 {
			fail("division by zero")
		}
		return VRealVal(x / y)
	case "%":
		if y == 0 {
			fail("division by zero")
		}
		return VRealVal(math.Mod(x, y))
	}
	fail("unknown operator %s", op)
	return VNullVal()
}

// likeMatch: % = any run, _ = one char, ASCII case-insensitive.
func likeMatch(s, pat string) bool {
	lower := func(c byte) byte {
		if c >= 'A' && c <= 'Z' {
			return c + 32
		}
		return c
	}
	si, pi := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pat) && pat[pi] == '%' {
			star, pi = pi, pi+1
			mark = si
		} else if pi < len(pat) && (pat[pi] == '_' || lower(pat[pi]) == lower(s[si])) {
			si++
			pi++
		} else if star >= 0 {
			pi = star + 1
			mark++
			si = mark
		} else {
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '%' {
		pi++
	}
	return pi == len(pat)
}

// canon produces a string used as a hash/group key: numbers are normalised
// so 1 and 1.0 group together.
func canon(v Value) string {
	switch v.Kind {
	case VNull:
		return "N"
	case VInt:
		return "n" + strconv.FormatInt(v.I, 10)
	case VReal:
		if v.D == math.Floor(v.D) && math.Abs(v.D) < 9e18 {
			return "n" + strconv.FormatInt(int64(v.D), 10)
		}
		return "r" + v.String()
	case VText:
		return "T" + v.S
	case VBool:
		if v.I != 0 {
			return "B1"
		}
		return "B0"
	}
	return "?"
}

func evalExpr(e *Expr, c *EvalCtx) Value {
	switch e.Kind {
	case EKLit:
		return e.Lit
	case EKCol:
		if e.Table == "new" || e.Table == "old" {
			if c.TrigTable == nil {
				fail("%s is only available inside a trigger", e.Table)
			}
			ci := c.TrigTable.ColIndex(e.Name)
			if ci < 0 {
				fail("no such column: %s.%s", e.Table, e.Name)
			}
			row := c.TrigNew
			if e.Table == "old" {
				row = c.TrigOld
			}
			if row == nil {
				fail("%s is not available for this trigger event", e.Table)
			}
			return row[ci]
		}
		// Bare identifiers in procedural/function expressions may refer to
		// function/procedure parameters when there is no current row.
		if e.Table == "" && c.Row == nil {
			if v, ok := c.lookupVar(e.Name); ok {
				return v
			}
			if v, ok := c.lookupVar("@" + e.Name); ok {
				return v
			}
		}

		ctx := c.outerN(e.OuterDepth)
		if ctx == nil || ctx.Row == nil || e.Idx < 0 || e.Idx >= len(ctx.Row) {
			fail("no such column: %s", exprToString(e))
		}
		return ctx.Row[e.Idx]
	case EKVar:
		if v, ok := c.lookupVar(e.Name); ok {
			return v
		}
		fail("undefined variable: %s", e.Name)
	case EKParam:
		fail("unbound parameter ?")
	case EKStar:
		fail("'*' is only valid inside COUNT(*)")
	case EKUnary:
		v := evalExpr(e.Args[0], c)
		if e.Op == "NOT" {
			t := truth(v)
			if t < 0 {
				return VNullVal()
			}
			return VBoolVal(t == 0)
		}
		if v.IsNull() {
			return v
		}
		if v.Kind == VInt {
			return VIntVal(-v.I)
		}
		if v.Kind == VReal {
			return VRealVal(-v.D)
		}
		fail("unary minus on a non-numeric value")
	case EKBinary:
		op := e.Op
		if op == "AND" {
			l := truth(evalExpr(e.Args[0], c))
			if l == 0 {
				return VBoolVal(false)
			}
			r := truth(evalExpr(e.Args[1], c))
			if r == 0 {
				return VBoolVal(false)
			}
			if l < 0 || r < 0 {
				return VNullVal()
			}
			return VBoolVal(true)
		}
		if op == "OR" {
			l := truth(evalExpr(e.Args[0], c))
			if l == 1 {
				return VBoolVal(true)
			}
			r := truth(evalExpr(e.Args[1], c))
			if r == 1 {
				return VBoolVal(true)
			}
			if l < 0 || r < 0 {
				return VNullVal()
			}
			return VBoolVal(false)
		}
		a, b := evalExpr(e.Args[0], c), evalExpr(e.Args[1], c)
		switch op {
		case "=", "!=", "<", "<=", ">", ">=":
			if a.IsNull() || b.IsNull() {
				return VNullVal()
			}
			r := cmpNonNull(a, b)
			var res bool
			switch op {
			case "=":
				res = r == 0
			case "!=":
				res = r != 0
			case "<":
				res = r < 0
			case "<=":
				res = r <= 0
			case ">":
				res = r > 0
			case ">=":
				res = r >= 0
			}
			return VBoolVal(res)
		}
		return arith(op, a, b)
	case EKFunc:
		if e.IsAgg {
			if c.Aggs == nil || e.Idx < 0 {
				fail("misuse of aggregate function %s()", e.Name)
			}
			return c.Aggs[e.Idx]
		}
		return callScalar(e, c)
	case EKIsNull:
		isnull := evalExpr(e.Args[0], c).IsNull()
		if e.Neg {
			return VBoolVal(!isnull)
		}
		return VBoolVal(isnull)
	case EKIn:
		x := evalExpr(e.Args[0], c)
		if x.IsNull() {
			return VNullVal()
		}
		sawNull, found := false, false
		for i := 1; i < len(e.Args) && !found; i++ {
			v := evalExpr(e.Args[i], c)
			if v.IsNull() {
				sawNull = true
			} else if cmpNonNull(x, v) == 0 {
				found = true
			}
		}
		if found {
			return VBoolVal(!e.Neg)
		}
		if sawNull {
			return VNullVal()
		}
		return VBoolVal(e.Neg)
	case EKBetween:
		x, lo, hi := evalExpr(e.Args[0], c), evalExpr(e.Args[1], c), evalExpr(e.Args[2], c)
		t1, t2 := -1, -1
		if !x.IsNull() && !lo.IsNull() {
			if cmpNonNull(x, lo) >= 0 {
				t1 = 1
			} else {
				t1 = 0
			}
		}
		if !x.IsNull() && !hi.IsNull() {
			if cmpNonNull(x, hi) <= 0 {
				t2 = 1
			} else {
				t2 = 0
			}
		}
		var t int
		if t1 == 0 || t2 == 0 {
			t = 0
		} else if t1 < 0 || t2 < 0 {
			t = -1
		} else {
			t = 1
		}
		if t < 0 {
			return VNullVal()
		}
		if e.Neg {
			t = 1 - t
		}
		return VBoolVal(t == 1)
	case EKLike:
		a, pat := evalExpr(e.Args[0], c), evalExpr(e.Args[1], c)
		if a.IsNull() || pat.IsNull() {
			return VNullVal()
		}
		if a.Kind != VText || pat.Kind != VText {
			fail("LIKE requires text operands")
		}
		m := likeMatch(a.S, pat.S)
		if e.Neg {
			m = !m
		}
		return VBoolVal(m)
	case EKCase:
		if e.CaseOperand != nil {
			v := evalExpr(e.CaseOperand, c)
			for i, cond := range e.WhenConds {
				cv := evalExpr(cond, c)
				if !v.IsNull() && !cv.IsNull() && cmpNonNull(v, cv) == 0 {
					return evalExpr(e.WhenVals[i], c)
				}
			}
		} else {
			for i, cond := range e.WhenConds {
				if truth(evalExpr(cond, c)) == 1 {
					return evalExpr(e.WhenVals[i], c)
				}
			}
		}
		if e.ElseVal != nil {
			return evalExpr(e.ElseVal, c)
		}
		return VNullVal()
	case EKCast:
		v := evalExpr(e.Args[0], c)
		return castValue(v, e.CastType)
	case EKInSub, EKExistsSub, EKScalarSub:
		if c.SubEval == nil {
			fail("subqueries are not supported in this context")
		}
		return c.SubEval(e, c)
	}
	fail("cannot evaluate expression")
	return VNullVal()
}

func castValue(v Value, t ColType) Value {
	if v.IsNull() {
		return v
	}
	switch t {
	case TInt:
		switch v.Kind {
		case VInt:
			return v
		case VReal:
			return VIntVal(int64(v.D))
		case VBool:
			return VIntVal(v.I)
		case VText:
			n, err := strconv.ParseInt(strings.TrimSpace(v.S), 10, 64)
			if err != nil {
				fail("cannot CAST '%s' to INT", v.S)
			}
			return VIntVal(n)
		}
	case TReal:
		switch v.Kind {
		case VReal:
			return v
		case VInt:
			return VRealVal(float64(v.I))
		case VText:
			f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64)
			if err != nil {
				fail("cannot CAST '%s' to REAL", v.S)
			}
			return VRealVal(f)
		}
	case TText:
		return VTextVal(v.String())
	case TBool:
		switch v.Kind {
		case VBool:
			return v
		case VInt:
			return VBoolVal(v.I != 0)
		}
	}
	fail("cannot CAST value to %s", t)
	return VNullVal()
}

func callScalar(e *Expr, c *EvalCtx) Value {
	f := e.Name
	a := make([]Value, len(e.Args))
	for i, x := range e.Args {
		a[i] = evalExpr(x, c)
	}
	argc := func(lo, hi int) {
		if len(a) < lo || len(a) > hi {
			fail("%s(): wrong number of arguments", f)
		}
	}
	switch f {
	case "upper", "lower":
		argc(1, 1)
		if a[0].IsNull() {
			return a[0]
		}
		s := a[0].String()
		if f == "upper" {
			return VTextVal(strings.ToUpper(s))
		}
		return VTextVal(strings.ToLower(s))
	case "length":
		argc(1, 1)
		if a[0].IsNull() {
			return a[0]
		}
		return VIntVal(int64(len(a[0].String())))
	case "abs":
		argc(1, 1)
		if a[0].IsNull() {
			return a[0]
		}
		if !a[0].IsNumeric() {
			fail("abs(): numeric argument required")
		}
		if a[0].Kind == VInt {
			if a[0].I < 0 {
				return VIntVal(-a[0].I)
			}
			return a[0]
		}
		return VRealVal(math.Abs(a[0].D))
	case "round":
		argc(1, 2)
		if a[0].IsNull() {
			return a[0]
		}
		if !a[0].IsNumeric() {
			fail("round(): numeric argument required")
		}
		digits := int64(0)
		if len(a) == 2 && a[1].Kind == VInt {
			digits = a[1].I
		}
		m := math.Pow(10, float64(digits))
		return VRealVal(math.Round(a[0].AsFloat()*m) / m)
	case "coalesce":
		argc(1, 64)
		for _, v := range a {
			if !v.IsNull() {
				return v
			}
		}
		return VNullVal()
	case "ifnull":
		argc(2, 2)
		if !a[0].IsNull() {
			return a[0]
		}
		return a[1]
	case "nullif":
		argc(2, 2)
		if !a[0].IsNull() && !a[1].IsNull() && cmpNonNull(a[0], a[1]) == 0 {
			return VNullVal()
		}
		return a[0]
	case "substr", "substring":
		argc(2, 3)
		if a[0].IsNull() || a[1].IsNull() {
			return VNullVal()
		}
		s := a[0].String()
		start := int64(1)
		if a[1].Kind == VInt {
			start = a[1].I
		}
		length := int64(len(s))
		if len(a) == 3 && a[2].Kind == VInt {
			length = a[2].I
		}
		if start < 1 {
			length += start - 1
			start = 1
		}
		if length <= 0 || int(start-1) >= len(s) {
			return VTextVal("")
		}
		end := int(start-1) + int(length)
		if end > len(s) {
			end = len(s)
		}
		return VTextVal(s[start-1 : end])
	case "trim":
		argc(1, 1)
		if a[0].IsNull() {
			return a[0]
		}
		return VTextVal(strings.TrimSpace(a[0].String()))
	case "concat":
		argc(1, 64)
		var sb strings.Builder
		for _, v := range a {
			if v.IsNull() {
				return VNullVal()
			}
			sb.WriteString(v.String())
		}
		return VTextVal(sb.String())
	default:
		if c.CallFunc != nil {
			if v, ok := c.CallFunc(f, a); ok {
				return v
			}
		}
		fail("no such function: %s", f)
	}
	return VNullVal()
}

// ---- aggregates
type AggState struct {
	Count  int64
	ISum   int64
	Sum    float64
	AllInt bool
	Has    bool
	Best   Value
	Seen   map[string]bool
}

func newAggState() *AggState { return &AggState{AllInt: true} }

func accumulate(st *AggState, agg *Expr, c *EvalCtx) {
	if agg.Args[0].Kind == EKStar {
		st.Count++
		return
	}
	v := evalExpr(agg.Args[0], c)
	if v.IsNull() {
		return
	}
	if agg.Distinct {
		if st.Seen == nil {
			st.Seen = map[string]bool{}
		}
		k := canon(v)
		if st.Seen[k] {
			return
		}
		st.Seen[k] = true
	}
	st.Count++
	switch agg.Name {
	case "sum", "avg":
		if !v.IsNumeric() {
			fail("%s(): numeric argument required", agg.Name)
		}
		if v.Kind == VInt {
			st.ISum = int64(uint64(st.ISum) + uint64(v.I))
		} else {
			st.AllInt = false
		}
		st.Sum += v.AsFloat()
	case "min", "max":
		if !st.Has {
			st.Best = v
		} else {
			cmp := cmpNonNull(v, st.Best)
			if (agg.Name == "min" && cmp < 0) || (agg.Name == "max" && cmp > 0) {
				st.Best = v
			}
		}
	}
	st.Has = true
}

func finalizeAgg(st *AggState, agg *Expr) Value {
	if agg.Name == "count" {
		return VIntVal(st.Count)
	}
	if !st.Has {
		return VNullVal()
	}
	if agg.Name == "sum" {
		if st.AllInt {
			return VIntVal(st.ISum)
		}
		return VRealVal(st.Sum)
	}
	if agg.Name == "avg" {
		return VRealVal(st.Sum / float64(st.Count))
	}
	return st.Best
}
