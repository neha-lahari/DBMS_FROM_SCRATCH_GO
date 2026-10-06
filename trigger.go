// trigger.go - CREATE/DROP TRIGGER and firing BEFORE/AFTER INSERT/UPDATE/DELETE.
package main

const maxTriggerDepth = 20

func (ex *Executor) createTrigger(s *Stmt) Result {
	ex.requireAdmin("CREATE TRIGGER")
	if _, ok := ex.cat.Triggers[s.Name]; ok {
		fail("trigger already exists: %s", s.Name)
	}
	if _, ok := ex.cat.Tables[s.Table]; !ok {
		fail("no such table: %s", s.Table)
	}
	td := &TriggerDef{Name: s.Name, Table: s.Table, Timing: s.TrigTiming, Event: s.TrigEvent, Body: s.RawText}
	ex.cat.PutTrigger(ex.tree, td)
	return msgResult("CREATE TRIGGER", 0)
}

func (ex *Executor) dropTrigger(s *Stmt) Result {
	ex.requireAdmin("DROP TRIGGER")
	if _, ok := ex.cat.Triggers[s.Name]; !ok {
		if s.IfExists {
			return msgResult("no such trigger", 0)
		}
		fail("no such trigger: %s", s.Name)
	}
	ex.cat.RemoveTrigger(ex.tree, s.Name)
	return msgResult("DROP TRIGGER", 0)
}

// fireTriggers runs every trigger matching (timing, event, table), in
// creation order. oldRow/newRow are pointers into the caller's row data:
// a BEFORE trigger's `SET NEW.col = ...` mutates *newRow in place so the
// change is visible to the insert/update that follows.
func (ex *Executor) fireTriggers(timing, event string, t *TableDef, oldRow, newRow *Row) {
	var names []string
	for _, n := range ex.cat.TrigOrder {
		td := ex.cat.Triggers[n]
		if td != nil && td.Table == t.Name && td.Timing == timing && td.Event == event {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return
	}
	ex.triggerDepth++
	if ex.triggerDepth > maxTriggerDepth {
		ex.triggerDepth--
		fail("trigger recursion limit exceeded on table %s", t.Name)
	}
	defer func() { ex.triggerDepth-- }()

	savedTable, savedOld, savedNew := ex.trigTable, ex.trigOld, ex.trigNew
	savedFrame := ex.frameVars
	defer func() {
		ex.trigTable, ex.trigOld, ex.trigNew = savedTable, savedOld, savedNew
		ex.frameVars = savedFrame
	}()
	ex.trigTable = t
	if oldRow != nil {
		ex.trigOld = *oldRow
	} else {
		ex.trigOld = nil
	}
	if newRow != nil {
		ex.trigNew = *newRow
	} else {
		ex.trigNew = nil
	}

	for _, n := range names {
		td := ex.cat.Triggers[n]
		ex.frameVars = map[string]Value{}
		body := ParseBodyText(td.Body)
		ex.execStmts(body)
	}

	if newRow != nil {
		*newRow = ex.trigNew
	}
}
