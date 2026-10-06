// main.go - interactive shell and `-c` / file-batch runner.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

func printTable(r *Result) {
	if len(r.Columns) == 0 {
		fmt.Println("(0 columns)")
		return
	}
	w := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		w[i] = len(c)
	}
	cells := make([][]string, len(r.Rows))
	for ri, row := range r.Rows {
		line := make([]string, len(row))
		for i, v := range row {
			s := "NULL"
			if !v.IsNull() {
				s = v.String()
			}
			if len(s) > w[i] {
				w[i] = len(s)
			}
			line[i] = s
		}
		cells[ri] = line
	}
	rule := func() {
		fmt.Print("+")
		for _, x := range w {
			fmt.Print(strings.Repeat("-", x+2) + "+")
		}
		fmt.Println()
	}
	printRow := func(v []string) {
		fmt.Print("|")
		for i, s := range v {
			fmt.Print(" " + s + strings.Repeat(" ", w[i]-len(s)+1) + "|")
		}
		fmt.Println()
	}
	rule()
	printRow(r.Columns)
	rule()
	for _, c := range cells {
		printRow(c)
	}
	rule()
	plural := "rows"
	if len(r.Rows) == 1 {
		plural = "row"
	}
	fmt.Printf("%d %s in set\n", len(r.Rows), plural)
}

func runOne(sess *Session, st *Stmt, timing bool) bool {
	t0 := time.Now()
	res, err := sess.Execute(st)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return false
	}
	if res.IsQuery {
		printTable(&res)
	} else {
		fmt.Println(res.Message)
	}
	if timing {
		fmt.Printf("(%.3f ms)\n", float64(time.Since(t0).Microseconds())/1000.0)
	}
	return true
}

func runBatch(sess *Session, sql string, timing bool) bool {
	ok := true
	script := NewSQLScript(sql)
	for !script.Done() {
		st, err := script.Next()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			ok = false
			continue
		}
		if !runOne(sess, &st, timing) {
			ok = false
		}
	}
	return ok
}

func dotCommand(sess *Session, line string, timing *bool) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	cmd := fields[0]
	switch cmd {
	case ".exit", ".quit":
		return false
	case ".help":
		fmt.Print(`.tables              list tables and views
.schema <table>      describe a table
.timing on|off       toggle query timing
.verify              check B+tree invariants and report row/page counts
.read <file>         run a .sql script
.user <name>         switch the current user (for GRANT/REVOKE testing)
.exit / .quit        leave the shell
`)
	case ".tables":
		runOne(sess, &Stmt{Kind: SShowTables}, false)
	case ".schema":
		if len(fields) < 2 {
			fmt.Fprintln(os.Stderr, "usage: .schema <table>")
			return true
		}
		runOne(sess, &Stmt{Kind: SDescribe, Table: fields[1]}, false)
	case ".timing":
		*timing = len(fields) > 1 && fields[1] == "on"
	case ".verify":
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintln(os.Stderr, "CORRUPT:", r)
				}
			}()
			n := sess.Verify()
			fmt.Printf("OK: %d entries, %d pages (%d free)\n", n, sess.PageCount(), sess.FreePageCount())
		}()
	case ".user":
		if len(fields) < 2 {
			fmt.Println(sess.CurrentUser())
			return true
		}
		if err := sess.SetUser(fields[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	case ".read":
		if len(fields) < 2 {
			fmt.Fprintln(os.Stderr, "usage: .read <file>")
			return true
		}
		data, err := os.ReadFile(fields[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: cannot open", fields[1])
			return true
		}
		runBatch(sess, string(data), *timing)
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd, "(try .help)")
	}
	return true
}

func main() {
	dbPath := "mydb.db"
	oneShot := ""
	timing := false
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 < len(args) {
				i++
				oneShot = args[i]
			}
		case "--timing":
			timing = true
		default:
			dbPath = args[i]
		}
	}

	sess, err := NewSession(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
	defer sess.Close()

	if oneShot != "" {
		if !runBatch(sess, oneShot, timing) {
			os.Exit(1)
		}
		return
	}

	fmt.Println("mydb - a SQL database in Go (type .help for commands, .exit to quit)")
	reader := bufio.NewReader(os.Stdin)
	stmtBuf := ""
	for {
		if stmtBuf == "" {
			fmt.Print("mydb> ")
		} else {
			fmt.Print("  ...> ")
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if stmtBuf == "" && strings.HasPrefix(trimmed, ".") {
			if !dotCommand(sess, trimmed, &timing) {
				break
			}
			continue
		}
		stmtBuf += line + "\n"
		if !strings.Contains(trimmed, ";") {
			continue // keep collecting a multi-line statement
		}
		script := NewSQLScript(stmtBuf)
		stmtBuf = ""
		for !script.Done() {
			st, err := script.Next()
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			runOne(sess, &st, timing)
		}
	}
	fmt.Println("bye")
}
