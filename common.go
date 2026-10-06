
package main

import (
	"encoding/binary"
	"fmt"
)

type DBError struct{ msg string }

func (e *DBError) Error() string { return e.msg }

func dbErrorf(format string, a ...interface{}) *DBError {
	return &DBError{msg: fmt.Sprintf(format, a...)}
}


func fail(format string, a ...interface{}) {
	panic(dbErrorf(format, a...))
}

// catchDBError recovers a panic(*DBError) into a normal error return.
// Any other panic (a real bug) is re-raised.
func catchDBError(errp *error) {
	if r := recover(); r != nil {
		if de, ok := r.(*DBError); ok {
			*errp = de
			return
		}
		panic(r)
	}
}

const (
	pageSize   = 4096
	maxKeySize = 1000 // keeps every B+tree node able to hold >= 2 entries
	maxValSize = 3000
)

// ---- big-endian helpers. Big-endian matters for encoded keys: byte order
// must equal numeric/lexicographic order for the B+tree's memcmp-style
// comparisons to behave like SQL ordering.

func putU8(buf *[]byte, v byte) { *buf = append(*buf, v) }

func putU16(buf *[]byte, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	*buf = append(*buf, b[:]...)
}

func putU32(buf *[]byte, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	*buf = append(*buf, b[:]...)
}

func putU64(buf *[]byte, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	*buf = append(*buf, b[:]...)
}

// reader is a bounds-checked sequential reader over a byte slice: a
// corrupt/truncated page throws a DBError instead of panicking with an
// index-out-of-range.
type reader struct {
	b   []byte
	pos int
}

func newReader(b []byte) *reader { return &reader{b: b} }

func (r *reader) need(n int) {
	if len(r.b)-r.pos < n {
		fail("corrupt data: unexpected end")
	}
}

func (r *reader) u8() byte {
	r.need(1)
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *reader) u16() uint16 {
	r.need(2)
	v := binary.BigEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}

func (r *reader) u32() uint32 {
	r.need(4)
	v := binary.BigEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

func (r *reader) u64() uint64 {
	r.need(8)
	v := binary.BigEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v
}

func (r *reader) str(n int) string {
	r.need(n)
	s := string(r.b[r.pos : r.pos+n])
	r.pos += n
	return s
}

func (r *reader) remaining() bool { return r.pos < len(r.b) }
