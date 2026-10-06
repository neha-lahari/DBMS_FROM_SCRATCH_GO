# mydb — a SQL database engine built from scratch in Go

A relational database management system implemented from first principles in
Go, following the design in *"Build Your Own Database From Scratch in Go"*
and extended to cover the full Senate-approved DBMS lab syllabus. No SQL
engine, storage library, or ORM is used anywhere — the on-disk B+tree, the
SQL parser, the query planner/executor, and the procedural (PL/SQL-like)
language are all original code in this repository.

## Building and running

```
go build -o mydb ./...
./mydb mydb.db                 # interactive shell
./mydb mydb.db -c "SELECT 1;"  # run one or more ';'-separated statements and exit
```

Inside the shell, `.help` lists the dot-commands (`.tables`, `.schema`,
`.verify`, `.user`, `.read <file>`, `.timing`, `.exit`).

Run the full feature-test script:

```
./mydb smoke.db
mydb> .read tests/smoke.sql
```

or non-interactively:

```
./mydb smoke.db -c "$(cat tests/smoke.sql)"
```

## Architecture

### Storage engine (from the book)

- **`pager.go`** — manages the database file as fixed-size 4KB pages. Commits
  are atomic and durable with *no write-ahead log*, using the book's
  technique: every committed transaction is copy-on-write (a changed page is
  never modified in place; a new page is allocated and the old one is
  queued onto a persisted free list), and the root of the whole tree is
  published by writing one of two alternating, checksummed meta pages,
  `fsync`ed before and after. A crash can only ever land on the old
  consistent image or the new one, never a torn mix of both. A free-list
  page chain means no space held by deleted/old data is ever lost. An
  `flock` on the file enforces single-writer access.
- **`btree.go`** — a classic copy-on-write B+tree over the pager: variable
  length keys/values, node splitting on overflow, merging/rebalancing on
  underflow, and a `Cursor` for ordered range scans. `Verify()` walks the
  whole tree checking every structural invariant (ordering, fanout, no
  cycles) — used by `.verify` and after the crash-recovery test below.
- **`value.go`** — SQL values (`INT`/`REAL`/`TEXT`/`BOOL`/`NULL`) and an
  **order-preserving binary encoding**: signed integers get their sign bit
  flipped, floats get IEEE-754 bit remapping, and text is escaped and
  NUL-terminated — all so plain byte-wise comparison in the B+tree produces
  the same ordering SQL expects, which is what makes primary-key and
  secondary-index range scans (`WHERE id BETWEEN ...`, `ORDER BY`) work
  without decoding every row.
- **`catalog.go`** — table/view/procedure/trigger/user/grant metadata, itself
  stored as rows in the same B+tree (namespaced key prefixes), so schema
  changes are transactional along with data.

### SQL front end

- **`lexer.go` / `parser.go`** — a hand-written lexer and recursive-descent
  parser for the full dialect below, including a **resilient,
  statement-by-statement parser** (`SQLScript`) so one typo in a 500-line
  script doesn't discard the other 499 lines — each bad statement is
  reported and skipped, everything else still runs.
- **`ast.go` / `bind.go` / `eval.go`** — the expression AST, name resolution
  (including correlated-subquery and trigger `NEW`/`OLD` resolution), and a
  three-valued-logic (NULL/true/false) expression evaluator with the usual
  arithmetic/comparison/`LIKE`/`CASE`/`CAST` operators and built-in
  functions.
- **`executor.go` / `select.go`** — DDL/DML execution, a query planner that
  picks a primary-key or secondary-index access path from `WHERE`-clause
  conditions when possible (see `EXPLAIN`), hash joins (when the `ON`
  clause has an equality) falling back to nested-loop joins, `GROUP BY` /
  `HAVING` / aggregates, `UNION`/`INTERSECT`/`EXCEPT`, and views (stored as
  raw SQL text and re-expanded on each use, so they always see current
  data).
- **`proc.go` / `trigger.go`** — one shared interpreter for the procedural
  mini-language, used uniformly by stored procedures, user-defined scalar
  functions (callable inside ordinary SQL expressions), and triggers.
- **`session.go`** — transactions (autocommit outside `BEGIN`, snapshot +
  rollback inside it) and dynamic SQL (`PREPARE`/`EXECUTE`/
  `EXECUTE IMMEDIATE`/`DEALLOCATE`) via a generic AST-walking `?`-placeholder
  substitution.

## Syllabus coverage

| # | Syllabus item | Where |
|---|---|---|
| 1 | DDL, DML, DCL | `CREATE/DROP/ALTER TABLE`, `INSERT/UPDATE/DELETE/SELECT`, `CREATE USER`, `GRANT`/`REVOKE`/`SHOW GRANTS` (`executor.go`, `dcl.go`) |
| 2 | Inbuilt functions | `upper lower length abs round coalesce ifnull nullif substr trim concat` + aggregates `COUNT SUM AVG MIN MAX` (`eval.go`) |
| 3 | Nested & join queries | scalar/`IN`/`EXISTS` subqueries (correlated too), `INNER`/`LEFT JOIN` via hash or nested-loop (`select.go`) |
| 4 | Set operators & views | `UNION`/`UNION ALL`/`INTERSECT`/`EXCEPT`, `CREATE/DROP VIEW` (`select.go`, `executor.go`) |
| 5 | Control structures | `IF/ELSIF/ELSE`, `WHILE`, `FOR @v IN lo..hi DO` inside procedures/functions/triggers (`proc.go`) |
| 6 | Procedures & functions | `CREATE PROCEDURE`, `CREATE FUNCTION` (+ `RETURN`), `CALL`, `DO ... END` blocks (`proc.go`) |
| 7 | Triggers | `CREATE TRIGGER ... BEFORE/AFTER INSERT/UPDATE/DELETE`, `NEW.col`/`OLD.col`, a `BEFORE` trigger mutating `NEW` is visible to the write that follows (`trigger.go`) |
| 8 | Dynamic & embedded SQL | `PREPARE ... FROM`, `EXECUTE ... USING`, `EXECUTE IMMEDIATE`, `DEALLOCATE`; `?` placeholders; `SELECT ... INTO @var` as the "embedded" host-variable binding (`session.go`) |

### Extras beyond the syllabus and the book

- Explicit transactions: `BEGIN` / `COMMIT` / `ROLLBACK`, with autocommit
  outside an explicit transaction.
- A real (if simplified) privilege system: one `admin` superuser plus
  per-table `GRANT`/`REVOKE`'d privileges enforced on every statement.
- `CASE`/`CAST` expressions, `LIKE`, `BETWEEN`, three-valued `NULL` logic.
- Secondary indexes (`CREATE INDEX`) used automatically by the query planner.
- `EXPLAIN` to show the chosen access path.
- Recursion guards on procedure/function calls and trigger chains, so a
  runaway recursive trigger or procedure fails cleanly instead of hanging
  or overflowing the stack.
- `.verify` — walks the on-disk B+tree and reports any structural
  corruption; used below to confirm crash safety.
- Resilient multi-statement script execution (`SQLScript`): a syntax error
  in one statement of a batch is reported and skipped without losing the
  statements before or after it.

## Crash-recovery / durability

Because every commit is copy-on-write with a checksummed, double-buffered
meta page and `fsync` before any page becomes visible, an interrupted
process (even `kill -9`) can only ever lose the single in-flight
transaction — never corrupt previously committed data. This was verified
directly: a batch of 50,000 single-row autocommit inserts was `SIGKILL`ed
mid-run, and on reopen `.verify` reported a structurally clean tree with
exactly the prefix of rows that had actually committed (no duplicates,
no gaps, no partial rows).

## Known limitations

- Single-writer (file-locked); no network protocol or concurrent clients.
- No query optimizer beyond single-table index selection — joins are
  executed in `FROM`/`JOIN` order.
- The procedural language re-parses a stored procedure/function/trigger
  body from its saved source text on every call rather than caching a
  compiled form; fine for lab-scale workloads, not optimized for
  high-frequency calls.

## Book coverage (Build Your Own Database From Scratch in Go, 2nd ed.)

| Ch | Where | Notes |
|---|---|---|
| 3-5 B+tree | `btree.go` | Book BNode page layout (type/nkeys, ptrs, offsets, klen/vlen KVs), 4KB pages, 1000/3000 B key/value limits, 1/4-page merge threshold, copy-on-write |
| 6-7 durability, free list | `pager.go` | No-WAL atomic commit via double fsync + meta flip. *Deviation:* two checksummed meta pages and a persisted free-list chain (not mmap / single meta / LNode head-tail list) |
| 8 tables on KV | `bookdb.go` | `@meta`/`@table` internal tables, 32-bit key prefixes, `Get/Insert/Update/Upsert/Delete`, update modes |
| 9 range queries | `btree_ext.go`, `bookdb.go` | `SeekLE`, `Prev`, `Last`, ascending and descending `Scanner`, order-preserving key encoding |
| 10 secondary indexes | `bookdb.go` | prefix + index cols + PK, index selection by covered columns, +/-infinity for missing columns, maintained atomically |
| 11 transactions | `kv.go` | `KVTX`: buffered pending writes, merged snapshot+pending iterator, atomic commit/abort |
| 12 concurrency | `kv.go` | Snapshot reads, optimistic conflict detection (read ranges vs committed-tx history), phantom detection, snapshot-safe page reuse |
| 13-14 query language | `parser.go` | `INDEX BY ... FILTER ...`, `LIMIT x, y` on SELECT (plus normal `WHERE`); UPDATE/DELETE accept INDEX BY/FILTER (no LIMIT) |

The book layer (`BDB`, `DBTX`, `KV`) is a separate API over its own database file; the SQL engine keeps its own catalog. Run `go test ./...` to exercise the book layer (snapshot isolation, conflicts, scans, index maintenance).
