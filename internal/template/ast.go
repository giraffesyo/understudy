package template

// tmplNode is one segment of a parsed template: literal text, an output
// expression ({{ ... }}), or a statement ({% ... %}).
type tmplNode interface{ tmplNode() }

type textNode struct{ text string }
type outputNode struct{ expr Expr }

func (textNode) tmplNode()   {}
func (outputNode) tmplNode() {}

// Expr is an expression AST node. Off is the rune offset in the template
// source, used to point error messages at the right spot.
type Expr interface{ exprOff() int }

type kwarg struct {
	name string
	val  Expr
}

type nameExpr struct {
	off  int
	name string
}

type literalExpr struct {
	off int
	val any
}

type getAttrExpr struct {
	off  int
	x    Expr
	name string
}

type getItemExpr struct {
	off   int
	x     Expr
	index Expr
}

type sliceExpr struct {
	off        int
	x          Expr
	lo, hi, st Expr // any may be nil
}

type callExpr struct {
	off    int
	fn     Expr
	args   []Expr
	kwargs []kwarg
}

type filterExpr struct {
	off    int
	x      Expr
	name   string
	full   string // the name as written (ansible.builtin.combine)
	args   []Expr
	kwargs []kwarg
}

type testExpr struct {
	off     int
	x       Expr
	name    string
	full    string // the name as written (ansible.builtin.combine)
	args    []Expr
	kwargs  []kwarg
	negated bool
}

type binExpr struct {
	off  int
	op   tokKind // arithmetic/comparison operator, or tokName for in/and/or
	opNm string  // "in", "not in", "and", "or" when op == tokName
	l, r Expr
}

type notExpr struct {
	off int
	x   Expr
}

type negExpr struct { // unary minus / plus
	off int
	neg bool
	x   Expr
}

type condExpr struct {
	off  int
	val  Expr
	cond Expr
	els  Expr // nil => Undefined when cond is false
}

type listExpr struct {
	off   int
	items []Expr
	tuple bool // a tuple literal ((a, b), ()), which evaluates as a list
}

type dictExpr struct {
	off  int
	keys []Expr
	vals []Expr
}

// compareExpr holds a (possibly chained) comparison: 1 < x <= 10.
type compareExpr struct {
	off   int
	first Expr
	ops   []string // "==", "!=", "<", "<=", ">", ">=", "in", "not in"
	rest  []Expr
}

func (e *nameExpr) exprOff() int    { return e.off }
func (e *literalExpr) exprOff() int { return e.off }
func (e *getAttrExpr) exprOff() int { return e.off }
func (e *getItemExpr) exprOff() int { return e.off }
func (e *sliceExpr) exprOff() int   { return e.off }
func (e *callExpr) exprOff() int    { return e.off }
func (e *filterExpr) exprOff() int  { return e.off }
func (e *testExpr) exprOff() int    { return e.off }
func (e *binExpr) exprOff() int     { return e.off }
func (e *notExpr) exprOff() int     { return e.off }
func (e *negExpr) exprOff() int     { return e.off }
func (e *condExpr) exprOff() int    { return e.off }
func (e *listExpr) exprOff() int    { return e.off }
func (e *dictExpr) exprOff() int    { return e.off }
func (e *compareExpr) exprOff() int { return e.off }
