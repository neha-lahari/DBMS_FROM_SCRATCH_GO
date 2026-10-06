package main

import "sort"

func (ex *Executor) createUser(s *Stmt) Result {
	ex.requireAdmin("CREATE USER")
	if _, ok := ex.cat.Users[s.Name]; ok {
		fail("user already exists: %s", s.Name)
	}
	ex.cat.PutUser(ex.tree, &UserDef{Name: s.Name, Password: s.Password})
	return msgResult("CREATE USER", 0)
}

func (ex *Executor) dropUser(s *Stmt) Result {
	ex.requireAdmin("DROP USER")
	if s.Name == "admin" {
		fail("cannot drop the admin user")
	}
	if _, ok := ex.cat.Users[s.Name]; !ok {
		if s.IfExists {
			return msgResult("no such user", 0)
		}
		fail("no such user: %s", s.Name)
	}
	ex.cat.RemoveUser(ex.tree, s.Name)
	return msgResult("DROP USER", 0)
}

func (ex *Executor) grantStmt(s *Stmt) Result {
	ex.requireAdmin("GRANT")
	if _, ok := ex.cat.Users[s.ToUser]; !ok {
		fail("no such user: %s", s.ToUser)
	}
	if s.OnTable != "*" {
		if _, ok := ex.cat.Tables[s.OnTable]; !ok {
			fail("no such table: %s", s.OnTable)
		}
	}
	g := ex.cat.GrantsFor(s.ToUser)
	g.grant(s.OnTable, s.Privs)
	ex.cat.PutGrants(ex.tree, g)
	return msgResult("GRANT", 0)
}

func (ex *Executor) revokeStmt(s *Stmt) Result {
	ex.requireAdmin("REVOKE")
	if _, ok := ex.cat.Users[s.ToUser]; !ok {
		fail("no such user: %s", s.ToUser)
	}
	g := ex.cat.GrantsFor(s.ToUser)
	g.revoke(s.OnTable, s.Privs)
	ex.cat.PutGrants(ex.tree, g)
	return msgResult("REVOKE", 0)
}

func (ex *Executor) showGrants(forUser string) Result {
	r := Result{IsQuery: true, Columns: []string{"user", "table", "privileges"}}
	if forUser != "" {
		if _, ok := ex.cat.Users[forUser]; !ok {
			fail("no such user: %s", forUser)
		}
	}
	var users []string
	for u := range ex.cat.Users {
		if forUser != "" && u != forUser {
			continue
		}
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		g, ok := ex.cat.Grants[u]
		if !ok {
			continue
		}
		var tables []string
		for t := range g.Privs {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		for _, t := range tables {
			var privs []string
			for p := range g.Privs[t] {
				privs = append(privs, p)
			}
			sort.Strings(privs)
			r.Rows = append(r.Rows, Row{VTextVal(u), VTextVal(t), VTextVal(joinStrs(privs, ", "))})
		}
	}
	return r
}

func joinStrs(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
