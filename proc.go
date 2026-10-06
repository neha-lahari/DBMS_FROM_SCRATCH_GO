package main

const maxCallDepth = 100

func (ex *Executor) createProc(s *Stmt) Result {
	ex.requireAdmin("CREATE PROCEDURE/FUNCTION")
	if _, ok := ex.cat.Procs[s.Name]; ok && !s.IfExists {
		fail("procedure or function already exists: %s", s.Name)
	}
	pd := &ProcDef{Name: s.Name, IsFunction: s.Kind == SCreateFunc, Params: s.Params, ReturnType: s.ReturnType, Body: s.RawText}
	ex.cat.PutProc(ex.tree, pd)
	kind := "PROCEDURE"
	if pd.IsFunction {
		kind = "FUNCTION"
	}
	return msgResult("CREATE "+kind, 0)
}

func (ex *Executor) dropProc(s *Stmt) Result {
	ex.requireAdmin("DROP PROCEDURE/FUNCTION")
	pd, ok := ex.cat.Procs[s.Name]
	wantFunc := s.Kind == SDropFunc
	if !ok || pd.IsFunction != wantFunc {
		if s.IfExists {
			return msgResult("no such procedure/function", 0)
		}
		fail("no such procedure or function: %s", s.Name)
	}
	ex.cat.RemoveProc(ex.tree, s.Name)
	kind := "PROCEDURE"
	if wantFunc {
		kind = "FUNCTION"
	}
	return msgResult("DROP "+kind, 0)
}

// callProcedure runs a stored procedure or function by name. callerCtx, if
// non-nil, is the EvalCtx the arguments should be evaluated in (a nested
// CALL from inside another procedure/trigger); nil means top level.
// Returns the function's RETURN value (VNullVal() for a plain procedure)
// and a Result suitable for the CALL statement.
func (ex *Executor) callProcedure(name string, argExprs []*Expr, callerCtx *EvalCtx) (Value, Result) {
	pd, ok := ex.cat.Procs[name]
	if !ok {
		fail("no such procedure or function: %s", name)
	}
	if len(argExprs) != len(pd.Params) {
		fail("%s expects %d argument(s), got %d", name, len(pd.Params), len(argExprs))
	}
	argVals := make([]Value, len(argExprs))
	evalCtx := callerCtx
	if evalCtx == nil {
		evalCtx = ex.ctx(nil, nil)
	}
	for i, ae := range argExprs {
		bindExpr(ae, nil, nil)
		argVals[i] = evalExpr(ae, evalCtx)
	}

	ex.callDepth++
	if ex.callDepth > maxCallDepth {
		ex.callDepth--
		fail("too much recursion calling %s", name)
	}
	defer func() { ex.callDepth-- }()

	newFrame := map[string]Value{}
	for i, p := range pd.Params {
		v := argVals[i]
		if !CoerceToType(&v, p.Type) {
			fail("argument %d to %s: type mismatch (expected %s)", i+1, name, p.Type)
		}
		newFrame[p.Name] = v
	}
	savedFrame := ex.frameVars
	savedResult := ex.procResult
	ex.frameVars = newFrame
	ex.procResult = Result{}
	defer func() { ex.frameVars = savedFrame; ex.procResult = savedResult }()

	body := ParseBodyText(pd.Body)
	returned, retVal := ex.execStmts(body)
	callResult := ex.procResult
	if pd.IsFunction && !returned {
		fail("function %s did not return a value", name)
	}
	kind := "PROCEDURE"
	if pd.IsFunction {
		kind = "FUNCTION"
	}
	if callResult.IsQuery {
		return retVal, callResult
	}
	return retVal, msgResult("CALL "+kind+" "+name, 0)
}

// callUserFunc is installed as EvalCtx.CallFunc so expression evaluation
// (eval.go) can invoke a user-defined FUNCTION by name without importing
// the executor.
func (ex *Executor) callUserFunc(name string, args []Value) (Value, bool) {
	pd, ok := ex.cat.Procs[name]
	if !ok || !pd.IsFunction {
		return VNullVal(), false
	}
	if len(args) != len(pd.Params) {
		fail("%s expects %d argument(s), got %d", name, len(pd.Params), len(args))
	}
	ex.callDepth++
	if ex.callDepth > maxCallDepth {
		ex.callDepth--
		fail("too much recursion calling %s", name)
	}
	defer func() { ex.callDepth-- }()

	newFrame := map[string]Value{}
	for i, p := range pd.Params {
		v := args[i]
		if !CoerceToType(&v, p.Type) {
			fail("argument %d to %s: type mismatch (expected %s)", i+1, name, p.Type)
		}
		newFrame[p.Name] = v
		// Parameters are normally stored as @name. Also expose the bare
		// name so function bodies can use RETURN a + b.
		if len(p.Name) > 1 && p.Name[0] == '@' {
			newFrame[p.Name[1:]] = v
		}
	}
	savedFrame := ex.frameVars
	ex.frameVars = newFrame
	defer func() { ex.frameVars = savedFrame }()

	body := ParseBodyText(pd.Body)
	returned, retVal := ex.execStmts(body)
	if !returned {
		fail("function %s did not return a value", name)
	}
	if !CoerceToType(&retVal, pd.ReturnType) {
		fail("function %s: return value type mismatch (expected %s)", name, pd.ReturnType)
	}
	return retVal, true
}

// execStmts runs a procedural block. Returns (true, value) the moment a
// RETURN is hit, propagating immediately out of every enclosing
// IF/WHILE/FOR/BEGIN block without running anything after it.
func (ex *Executor) execStmts(body []PStmt) (bool, Value) {
	for i := range body {
		if ret, val := ex.execOne(&body[i]); ret {
			return true, val
		}
	}
	return false, VNullVal()
}

func (ex *Executor) execOne(p *PStmt) (bool, Value) {
	switch p.Kind {
	case PSql:
		ex.procResult = ex.Run(p.SQL)
		return false, VNullVal()
	case PDeclare:
		v := VNullVal()
		if p.Default != nil {
			v = evalExpr(p.Default, ex.ctx(nil, nil))
		}
		if !v.IsNull() && !CoerceToType(&v, p.VarType) {
			fail("DECLARE %s: type mismatch", p.VarName)
		}
		ex.ensureFrame()
		ex.frameVars[p.VarName] = v
		return false, VNullVal()
	case PSet:
		v := evalExpr(p.Value, ex.ctx(nil, nil))
		ex.assignTarget(p.Target, v)
		return false, VNullVal()
	case PIf:
		if truth(evalExpr(p.Cond, ex.ctx(nil, nil))) == 1 {
			return ex.execStmts(p.Body)
		}
		for _, ei := range p.ElseIfs {
			if truth(evalExpr(ei.Cond, ex.ctx(nil, nil))) == 1 {
				return ex.execStmts(ei.Body)
			}
		}
		if p.Else != nil {
			return ex.execStmts(p.Else)
		}
		return false, VNullVal()
	case PWhile:
		guard := 0
		for truth(evalExpr(p.Cond, ex.ctx(nil, nil))) == 1 {
			if ret, val := ex.execStmts(p.Body); ret {
				return true, val
			}
			guard++
			if guard > 10_000_000 {
				fail("WHILE loop exceeded the iteration limit (possible infinite loop)")
			}
		}
		return false, VNullVal()
	case PForRange:
		lo := evalExpr(p.LoVal, ex.ctx(nil, nil))
		hi := evalExpr(p.HiVal, ex.ctx(nil, nil))
		if lo.Kind != VInt || hi.Kind != VInt {
			fail("FOR ... IN range bounds must be integers")
		}
		ex.ensureFrame()
		for i := lo.I; i <= hi.I; i++ {
			ex.frameVars[p.VarName] = VIntVal(i)
			if ret, val := ex.execStmts(p.Body); ret {
				return true, val
			}
		}
		return false, VNullVal()
	case PReturn:
		v := VNullVal()
		if p.Value != nil {
			v = evalExpr(p.Value, ex.ctx(nil, nil))
		}
		return true, v
	case PCall:
		ex.callProcedure(p.CallName, p.CallArgs, ex.ctx(nil, nil))
		return false, VNullVal()
	case PBlock:
		return ex.execStmts(p.Body)
	case PRaise:
		msg := evalExpr(p.Value, ex.ctx(nil, nil))
		fail("%s", msg.String())
	}
	return false, VNullVal()
}

func (ex *Executor) ensureFrame() {
	if ex.frameVars == nil {
		ex.frameVars = map[string]Value{}
	}
}

// assignTarget handles SET @var = ... and SET NEW.col = ... / OLD.col = ...
func (ex *Executor) assignTarget(target string, v Value) {
	if len(target) > 0 && target[0] == '@' {
		ex.ensureFrame()
		ex.frameVars[target] = v
		return
	}
	dot := -1
	for i := 0; i < len(target); i++ {
		if target[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		fail("invalid assignment target: %s", target)
	}
	tbl, col := target[:dot], target[dot+1:]
	if ex.trigTable == nil {
		fail("%s is only available inside a trigger", tbl)
	}
	if tbl == "old" {
		fail("OLD is read-only")
	}
	ci := ex.trigTable.ColIndex(col)
	if ci < 0 {
		fail("no such column: %s.%s", tbl, col)
	}
	if ex.trigNew == nil {
		fail("NEW is not available for this trigger event")
	}
	if !CoerceToType(&v, ex.trigTable.Cols[ci].Type) {
		fail("type mismatch assigning to NEW.%s", col)
	}
	ex.trigNew[ci] = v
}

// RunDoBlock executes a top-level DO BEGIN ... END ad-hoc script with a
// fresh variable frame (used by the SDoBlock statement).
func (ex *Executor) RunDoBlock(body []PStmt) Result {
	saved := ex.frameVars
	ex.frameVars = map[string]Value{}
	defer func() { ex.frameVars = saved }()
	ex.execStmts(body)
	return msgResult("DO", 0)
}
