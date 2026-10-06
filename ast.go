
package main

type ExprKind int

const (
	EKLit ExprKind = iota
	EKCol
	EKVar  
	EKParam 
	EKUnary
	EKBinary
	EKFunc
	EKStar
	EKIsNull
	EKIn
	EKInSub // expr [NOT] IN (subquery)
	EKExistsSub
	EKScalarSub
	EKBetween
	EKLike
	EKCase
	EKCast
)

type Expr struct {
	Kind ExprKind

	Op  string // Unary: "-" "NOT"; Binary: + - * / % = != < <= > >= AND OR ||
	Lit Value  // Lit

	Table, Name string // Col: optional qualifier + column; Func/Var name
	Args        []*Expr

	Neg      bool // IS NOT NULL / NOT IN / NOT BETWEEN / NOT LIKE
	Distinct bool // COUNT(DISTINCT x)
	IsAgg    bool // count/sum/avg/min/max

	Idx        int
	OuterDepth int 

	Sub *SelectStmt // EKInSub / EKExistsSub / EKScalarSub

	// EKCase
	CaseOperand *Expr // optional: CASE x WHEN ...
	WhenConds   []*Expr
	WhenVals    []*Expr
	ElseVal     *Expr

	// EKCast
	CastType ColType
}

func litExpr(v Value) *Expr { return &Expr{Kind: EKLit, Lit: v} }

type ColDef struct {
	Name    string
	Type    ColType
	NotNull bool
	PK      bool
	Unique  bool
	Default *Expr
}

type SelectItem struct {
	Expr      *Expr
	Alias     string
	Star      bool
	StarTable string // for t.*
}

type TableRef struct {
	Name, Alias string
	// Sub is set for a derived table: FROM (SELECT ...) alias
	Sub *SelectStmt
}

type JoinClause struct {
	Left  bool
	Right bool
	Full  bool
	Ref   TableRef
	On    *Expr
}

type OrderItem struct {
	Expr *Expr
	Desc bool
}

type SetOp int

const (
	SetOpNone SetOp = iota
	SetOpUnion
	SetOpUnionAll
	SetOpIntersect
	SetOpExcept
)

type SelectStmt struct {
	Distinct bool
	Items    []SelectItem
	IntoVars []string // SELECT ... INTO @v1, @v2 FROM ... (used inside procedures)
	HasFrom  bool
	From     TableRef
	Joins    []JoinClause
	Where    *Expr
	GroupBy  []*Expr
	Having   *Expr
	OrderBy  []OrderItem
	Limit    int64 // -1 = unset
	Offset   int64

	SetOp    SetOp
	Combined *SelectStmt
}


type PKind int

const (
	PSql PKind = iota
	PDeclare
	PSet
	PIf
	PWhile
	PForRange
	PReturn
	PCall
	PBlock
	PRaise
)

type ElseIf struct {
	Cond *Expr
	Body []PStmt
}

type PStmt struct {
	Kind PKind

	SQL *Stmt // PSql: a plain DDL/DML/SELECT statement

	VarName string  // PDeclare, PForRange (loop variable, without '@')
	VarType ColType // PDeclare
	Default *Expr   // PDeclare

	Target string // PSet: "@x" or "new.col" / "old.col"
	Value  *Expr  // PSet, PReturn (return value), PRaise (message expr)

	Cond    *Expr   // PIf condition, PWhile condition
	Body    []PStmt // PIf: THEN body; PWhile/PForRange: loop body
	ElseIfs []ElseIf
	Else    []PStmt

	LoVal, HiVal *Expr // PForRange

	CallName string
	CallArgs []*Expr
}

// ---- DDL/DML/DCL top-level statements
type StmtKind int

const (
	SCreateTable StmtKind = iota
	SDropTable
	SAlterTable
	SCreateDatabase
	SUseDatabase
	SCreateIndex
	SDropIndex
	SInsert
	SSelect
	SUpdate
	SDelete
	SBegin
	SCommit
	SRollback
	SShowTables
	SDescribe
	SCreateView
	SDropView
	SCreateProc
	SDropProc
	SCreateFunc
	SDropFunc
	SCall
	SCreateTrigger
	SDropTrigger
	SCreateUser
	SDropUser
	SGrant
	SRevoke
	SShowGrants
	SPrepare
	SExecute
	SExecuteImmediate
	SDeallocate
	SDoBlock
)

type Param struct {
	Name string
	Type ColType
}

type Stmt struct {
	Kind    StmtKind
	Explain bool

	IfNotExists, IfExists bool
	Table, Name           string  // target table / object name
	AlterAdd              *ColDef // ALTER TABLE ... ADD COLUMN
	Database              string  // CREATE DATABASE / USE

	ColDefs []ColDef   // CREATE TABLE
	PkCols  []string   // table-level PRIMARY KEY (...)
	Uniques [][]string // table-level UNIQUE (...)

	Unique  bool     // CREATE UNIQUE INDEX
	IdxCols []string // CREATE INDEX

	InsertCols []string
	Rows       [][]*Expr   // INSERT ... VALUES
	InsertSel  *SelectStmt // INSERT INTO t SELECT ...

	Sets  []SetClause // UPDATE ... SET
	Where *Expr       // UPDATE / DELETE

	Select SelectStmt // SELECT / CREATE VIEW ... AS SELECT

	// CREATE PROCEDURE / FUNCTION
	Params     []Param
	ReturnType ColType
	Body       []PStmt

	// CALL
	CallArgs []*Expr

	// CREATE TRIGGER
	TrigTiming string // "BEFORE" / "AFTER"
	TrigEvent  string // "INSERT" / "UPDATE" / "DELETE"

	// GRANT/REVOKE
	Privs   []string // "SELECT","INSERT","UPDATE","DELETE","ALL"
	OnTable string
	ToUser  string

	Password string

	// PREPARE/EXECUTE
	PrepSQL  string
	ExecArgs []*Expr

	RawText string
}

type SetClause struct {
	Col   string
	Value *Expr
}
