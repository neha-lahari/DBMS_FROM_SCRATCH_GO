-- ============================================================================
-- smoke.sql - exercises every item in the Senate-approved lab syllabus plus
-- the book's storage-engine features. Run with:
--   ./mydb smoke.db -c "$(cat tests/smoke.sql)"
-- or interactively:
--   ./mydb smoke.db
--   mydb> .read tests/smoke.sql
-- ============================================================================

-- ---------------------------------------------------------------------------
-- 1. DDL, DML and DCL
-- ---------------------------------------------------------------------------
CREATE TABLE dept (id INT PRIMARY KEY, name TEXT);
CREATE TABLE emp (id INT PRIMARY KEY, name TEXT, dept_id INT, salary REAL);
CREATE INDEX idx_emp_dept ON emp(dept_id);

INSERT INTO dept VALUES (1,'Engineering'),(2,'Sales'),(3,'HR');
INSERT INTO emp VALUES
  (1,'Alice',1,90000),(2,'Bob',1,85000),(3,'Carol',2,70000),
  (4,'Dan',2,72000),(5,'Eve',NULL,60000);

SELECT * FROM emp WHERE dept_id = 1 ORDER BY salary DESC;
UPDATE emp SET salary = salary * 1.10 WHERE dept_id = 1;
DELETE FROM emp WHERE id = 5;
SELECT COUNT(*) FROM emp;

CREATE USER bob_user;
GRANT SELECT ON emp TO bob_user;
SHOW GRANTS FOR bob_user;
REVOKE SELECT ON emp FROM bob_user;
DROP USER bob_user;

-- ---------------------------------------------------------------------------
-- 2. Inbuilt functions
-- ---------------------------------------------------------------------------
SELECT upper(name), lower(name), length(name) FROM emp WHERE id = 1;
SELECT round(salary, -3), abs(-5), coalesce(NULL, 'fallback'), ifnull(NULL, 42) FROM emp WHERE id = 1;
SELECT substr(name, 1, 3), trim('  padded  '), concat(name, '@company.com') FROM emp WHERE id = 1;
SELECT COUNT(*), SUM(salary), AVG(salary), MIN(salary), MAX(salary) FROM emp;

-- ---------------------------------------------------------------------------
-- 3. Nested queries & join queries
-- ---------------------------------------------------------------------------
SELECT name FROM emp WHERE salary > (SELECT AVG(salary) FROM emp);
SELECT name FROM dept d WHERE EXISTS (SELECT 1 FROM emp e WHERE e.dept_id = d.id);
SELECT name FROM dept WHERE id IN (SELECT dept_id FROM emp WHERE salary > 80000);

SELECT e.name, d.name AS dept FROM emp e INNER JOIN dept d ON e.dept_id = d.id ORDER BY e.id;
SELECT e.name, d.name AS dept FROM emp e LEFT JOIN dept d ON e.dept_id = d.id ORDER BY e.id;

-- ---------------------------------------------------------------------------
-- 4. Set operators & views
-- ---------------------------------------------------------------------------
SELECT name FROM dept UNION SELECT name FROM emp;
SELECT name FROM dept UNION ALL SELECT name FROM emp;
SELECT id FROM dept INTERSECT SELECT dept_id FROM emp;
SELECT id FROM dept EXCEPT SELECT dept_id FROM emp;

CREATE VIEW high_earners AS SELECT name, salary FROM emp WHERE salary > 70000;
SELECT * FROM high_earners ORDER BY salary DESC;
DROP VIEW high_earners;

-- GROUP BY / HAVING (not a separate syllabus line, but core RDBMS and used above)
SELECT dept_id, COUNT(*) AS n, AVG(salary) AS avg_sal
  FROM emp GROUP BY dept_id HAVING COUNT(*) >= 1 ORDER BY dept_id;

-- ---------------------------------------------------------------------------
-- 5. Control structures (procedural language: IF/ELSIF/ELSE, WHILE, FOR)
-- ---------------------------------------------------------------------------
CREATE FUNCTION bonus_tier(@salary REAL) RETURNS TEXT
BEGIN
  IF @salary >= 90000 THEN
    RETURN 'gold';
  ELSIF @salary >= 75000 THEN
    RETURN 'silver';
  ELSE
    RETURN 'bronze';
  END IF;
END;
SELECT name, salary, bonus_tier(salary) FROM emp ORDER BY salary DESC;

CREATE TABLE counters (n INT PRIMARY KEY, sq INT);
CREATE PROCEDURE fill_squares(@upto INT)
BEGIN
  DECLARE @i INT DEFAULT 1;
  WHILE @i <= @upto DO
    INSERT INTO counters VALUES (@i, @i * @i);
    SET @i = @i + 1;
  END WHILE;
END;
CALL fill_squares(5);

CREATE PROCEDURE fill_range()
BEGIN
  FOR @k IN 10..13 DO
    INSERT INTO counters VALUES (@k, @k * 2);
  END FOR;
END;
CALL fill_range();
SELECT * FROM counters ORDER BY n;

-- ---------------------------------------------------------------------------
-- 6. Procedures and functions (CALL, RETURN, nesting, DO blocks)
-- ---------------------------------------------------------------------------
CREATE PROCEDURE give_raise(@emp_id INT, @pct REAL)
BEGIN
  UPDATE emp SET salary = salary * (1 + @pct / 100) WHERE id = @emp_id;
END;
CALL give_raise(2, 5);
SELECT name, salary FROM emp WHERE id = 2;

CREATE FUNCTION total_payroll() RETURNS REAL
BEGIN
  DECLARE @total REAL;
  SELECT SUM(salary) INTO @total FROM emp;
  RETURN @total;
END;
SELECT total_payroll() AS total_payroll;

-- ---------------------------------------------------------------------------
-- 7. Triggers (BEFORE/AFTER, NEW/OLD)
-- ---------------------------------------------------------------------------
CREATE TABLE accounts (id INT PRIMARY KEY, name TEXT, balance REAL);
CREATE TABLE audit_log (id INT PRIMARY KEY, msg TEXT);

CREATE PROCEDURE log_it(@m TEXT)
BEGIN
  DECLARE @n INT;
  SELECT COUNT(*) INTO @n FROM audit_log;
  INSERT INTO audit_log VALUES (@n + 1, @m);
END;

CREATE TRIGGER trg_before_ins BEFORE INSERT ON accounts
BEGIN
  IF NEW.balance < 0 THEN
    SET NEW.balance = 0;
  END IF;
END;

CREATE TRIGGER trg_after_upd AFTER UPDATE ON accounts
BEGIN
  CALL log_it('balance changed for account ' || OLD.id);
END;

INSERT INTO accounts VALUES (1, 'Alice', -50);   -- BEFORE trigger clamps this to 0
SELECT * FROM accounts;
UPDATE accounts SET balance = 100 WHERE id = 1;  -- AFTER trigger logs it
SELECT * FROM audit_log;

-- ---------------------------------------------------------------------------
-- 8. Dynamic & embedded SQL (PREPARE / EXECUTE / EXECUTE IMMEDIATE)
-- ---------------------------------------------------------------------------
PREPARE ins_emp FROM 'INSERT INTO emp VALUES (?, ?, ?, ?)';
EXECUTE ins_emp USING 6, 'Frank', 3, 55000;
EXECUTE ins_emp USING 7, 'Grace', 3, 58000;
DEALLOCATE ins_emp;
SELECT * FROM emp WHERE dept_id = 3 ORDER BY id;

EXECUTE IMMEDIATE 'DELETE FROM emp WHERE id = 7';
SELECT COUNT(*) FROM emp;

-- ---------------------------------------------------------------------------
-- Extras: explicit transactions, EXPLAIN, CAST, CASE
-- ---------------------------------------------------------------------------
BEGIN;
INSERT INTO emp VALUES (8, 'Henry', 1, 99000);
ROLLBACK;
SELECT COUNT(*) FROM emp;  -- Henry should NOT be present

BEGIN;
INSERT INTO emp VALUES (8, 'Henry', 1, 99000);
COMMIT;
SELECT COUNT(*) FROM emp;  -- Henry present now

EXPLAIN SELECT * FROM emp WHERE id = 1;

SELECT name,
  CASE WHEN salary >= 90000 THEN 'high' WHEN salary >= 70000 THEN 'mid' ELSE 'low' END AS band,
  CAST(salary AS INT) AS salary_int
FROM emp ORDER BY id;
