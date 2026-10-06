// value.go - SQL values, and how they are turned into bytes.
package main

import (
	"fmt"
	"math"
	"strconv"
)

type ColType uint8

const (
	TInt ColType = iota + 1
	TReal
	TText
	TBool
)

func (t ColType) String() string {
	switch t {
	case TInt:
		return "INT"
	case TReal:
		return "REAL"
	case TText:
		return "TEXT"
	case TBool:
		return "BOOL"
	}
	return "?"
}

type ValueKind uint8

const (
	VNull ValueKind = iota
	VInt
	VReal
	VText
	VBool
)

// Value is a dynamically-typed SQL scalar. Zero value is NULL.
type Value struct {
	Kind ValueKind
	I    int64 // Int, Bool (0/1)
	D    float64
	S    string
}

func VNullVal() Value          { return Value{Kind: VNull} }
func VIntVal(v int64) Value    { return Value{Kind: VInt, I: v} }
func VRealVal(v float64) Value { return Value{Kind: VReal, D: v} }
func VTextVal(v string) Value  { return Value{Kind: VText, S: v} }
func VBoolVal(v bool) Value {
	i := int64(0)
	if v {
		i = 1
	}
	return Value{Kind: VBool, I: i}
}

func (v Value) IsNull() bool    { return v.Kind == VNull }
func (v Value) IsNumeric() bool { return v.Kind == VInt || v.Kind == VReal }
func (v Value) AsFloat() float64 {
	if v.Kind == VInt {
		return float64(v.I)
	}
	return v.D
}

func (v Value) String() string {
	switch v.Kind {
	case VNull:
		return "NULL"
	case VInt:
		return strconv.FormatInt(v.I, 10)
	case VBool:
		if v.I != 0 {
			return "true"
		}
		return "false"
	case VText:
		return v.S
	case VReal:
		if math.IsNaN(v.D) {
			return "nan"
		}
		if math.IsInf(v.D, 1) {
			return "inf"
		}
		if math.IsInf(v.D, -1) {
			return "-inf"
		}
		s := strconv.FormatFloat(v.D, 'g', 15, 64)
		hasDotOrE := false
		for _, c := range s {
			if c == '.' || c == 'e' {
				hasDotOrE = true
				break
			}
		}
		if !hasDotOrE {
			s += ".0"
		}
		return s
	}
	return "?"
}

func rank(v Value) int {
	switch v.Kind {
	case VNull:
		return 0
	case VBool:
		return 1
	case VInt, VReal:
		return 2
	default:
		return 3
	}
}

// CompareValues gives the total order used by ORDER BY / DISTINCT / GROUP BY:
// NULL < bool < numbers < text.
func CompareValues(a, b Value) int {
	ra, rb := rank(a), rank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ra {
	case 0:
		return 0
	case 1:
		return cmpI64(a.I, b.I)
	case 2:
		if a.Kind == VInt && b.Kind == VInt {
			return cmpI64(a.I, b.I)
		}
		return cmpF64(a.AsFloat(), b.AsFloat())
	default:
		if a.S < b.S {
			return -1
		} else if a.S > b.S {
			return 1
		}
		return 0
	}
}

func cmpI64(a, b int64) int {
	if a < b {
		return -1
	} else if a > b {
		return 1
	}
	return 0
}
func cmpF64(a, b float64) int {
	if a < b {
		return -1
	} else if a > b {
		return 1
	}
	return 0
}

// CoerceToType converts v in-place so it can be stored in a column of type
// t, returning false if the conversion is not possible.
func CoerceToType(v *Value, t ColType) bool {
	if v.IsNull() {
		return true
	}
	switch t {
	case TInt:
		return v.Kind == VInt
	case TReal:
		if v.Kind == VReal {
			return true
		}
		if v.Kind == VInt {
			*v = VRealVal(float64(v.I))
			return true
		}
		return false
	case TText:
		return v.Kind == VText
	case TBool:
		if v.Kind == VBool {
			return true
		}
		if v.Kind == VInt && (v.I == 0 || v.I == 1) {
			*v = VBoolVal(v.I == 1)
			return true
		}
		return false
	}
	return false
}

// EncodeKeyPart appends an order-preserving encoding of v (typed as t) to
// out: comparing two encoded byte strings lexicographically gives the same
// result as comparing the original values. Each part starts with a marker
// byte (0 = NULL, 1 = value), and composite keys are formed by
// concatenating parts, so text must be escaped/terminated to stay
// prefix-free.
func EncodeKeyPart(out *[]byte, v Value, t ColType) {
	if v.IsNull() {
		putU8(out, 0)
		return
	}
	putU8(out, 1)
	switch t {
	case TInt:
		putU64(out, uint64(v.I)^(1<<63)) // flip sign bit: negatives sort first
	case TReal:
		bits := math.Float64bits(v.D)
		if bits>>63 != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		putU64(out, bits)
	case TBool:
		if v.I != 0 {
			putU8(out, 1)
		} else {
			putU8(out, 0)
		}
	case TText:
		for i := 0; i < len(v.S); i++ {
			c := v.S[i]
			if c == 0 {
				putU8(out, 1)
				putU8(out, 1)
			} else if c == 1 {
				putU8(out, 1)
				putU8(out, 2)
			} else {
				*out = append(*out, c)
			}
		}
		putU8(out, 0) // terminator sorts before any escaped byte
	}
}

// EncodeRow appends a compact, self-describing encoding of row to out.
func EncodeRow(out *[]byte, row []Value) {
	for _, v := range row {
		switch v.Kind {
		case VNull:
			putU8(out, 0)
		case VInt:
			putU8(out, 1)
			putU64(out, uint64(v.I))
		case VReal:
			putU8(out, 2)
			putU64(out, math.Float64bits(v.D))
		case VText:
			putU8(out, 3)
			putU32(out, uint32(len(v.S)))
			*out = append(*out, v.S...)
		case VBool:
			putU8(out, 4)
			if v.I != 0 {
				putU8(out, 1)
			} else {
				putU8(out, 0)
			}
		}
	}
}

func DecodeRow(in string, ncols int) []Value {
	r := newReader([]byte(in))
	row := make([]Value, 0, ncols)
	for c := 0; c < ncols; c++ {
		switch r.u8() {
		case 0:
			row = append(row, VNullVal())
		case 1:
			row = append(row, VIntVal(int64(r.u64())))
		case 2:
			row = append(row, VRealVal(math.Float64frombits(r.u64())))
		case 3:
			n := int(r.u32())
			row = append(row, VTextVal(r.str(n)))
		case 4:
			row = append(row, VBoolVal(r.u8() != 0))
		default:
			fail("corrupt row")
		}
	}
	return row
}

// PrefixSuccessor returns the smallest byte string greater than every
// string that starts with prefix -- used to turn a "starts with" condition
// into a half-open [lo, hi) scan range.
func PrefixSuccessor(prefix []byte) []byte {
	s := append([]byte(nil), prefix...)
	for len(s) > 0 && s[len(s)-1] == 0xFF {
		s = s[:len(s)-1]
	}
	if len(s) > 0 {
		s[len(s)-1]++
	}
	return s
}

var _ = fmt.Sprintf
