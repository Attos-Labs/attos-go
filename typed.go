package attos

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unsafe"
)

// FieldType is the type of one field of a Schema.
type FieldType int

const (
	U8 FieldType = iota
	U16
	U32
	U64
	I8
	I16
	I32
	I64
	F32
	F64
	Bool
	String // u16 big-endian length, then UTF-8 bytes
	Bytes  // u16 big-endian length, then bytes
)

// ErrSchema is returned for values that do not fit their Schema.
var ErrSchema = errors.New("typed value does not match schema")

// Field names one typed field of a value.
type Field struct {
	Name string
	Type FieldType
}

// Schema describes a structured value (a client-side convention; the file
// format is unchanged). Fields are laid out in declaration order; integers
// and floats are big-endian, Bool is one byte (0 or 1), String and Bytes are a
// u16 big-endian length followed by the content. The Rust and Java SDKs use the
// same layout.
type Schema struct {
	fields []Field
}

// NewSchema returns a Schema for fields in declaration order.
func NewSchema(fields ...Field) *Schema { return &Schema{fields: fields} }

var fixedWidth = map[FieldType]int{U8: 1, I8: 1, Bool: 1, U16: 2, I16: 2, U32: 4, I32: 4, F32: 4, U64: 8, I64: 8, F64: 8}

func schemaErr(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrSchema}, a...)...)
}

// locate returns the field index and the offset of its first byte in v.
func (sc *Schema) locate(v []byte, name string) (int, int, error) {
	pos := 0
	for i, f := range sc.fields {
		if f.Name == name {
			return i, pos, nil
		}
		if w, ok := fixedWidth[f.Type]; ok {
			pos += w
			continue
		}
		if pos+2 > len(v) {
			return 0, 0, schemaErr("value truncated")
		}
		pos += 2 + int(binary.BigEndian.Uint16(v[pos:]))
	}
	return 0, 0, schemaErr("no field named %q", name)
}

func (sc *Schema) fixed(v []byte, name string, want ...FieldType) ([]byte, FieldType, error) {
	i, pos, err := sc.locate(v, name)
	if err != nil {
		return nil, 0, err
	}
	t := sc.fields[i].Type
	ok := false
	for _, w := range want {
		ok = ok || w == t
	}
	if !ok {
		return nil, 0, schemaErr("field %q has another type", name)
	}
	w := fixedWidth[t]
	if pos+w > len(v) {
		return nil, 0, schemaErr("value truncated")
	}
	return v[pos : pos+w], t, nil
}

// Uint reads a U8, U16, U32 or U64 field.
func (sc *Schema) Uint(v []byte, name string) (uint64, error) {
	b, t, err := sc.fixed(v, name, U8, U16, U32, U64)
	if err != nil {
		return 0, err
	}
	switch t {
	case U8:
		return uint64(b[0]), nil
	case U16:
		return uint64(binary.BigEndian.Uint16(b)), nil
	case U32:
		return uint64(binary.BigEndian.Uint32(b)), nil
	}
	return binary.BigEndian.Uint64(b), nil
}

// Int reads an I8, I16, I32 or I64 field.
func (sc *Schema) Int(v []byte, name string) (int64, error) {
	b, t, err := sc.fixed(v, name, I8, I16, I32, I64)
	if err != nil {
		return 0, err
	}
	switch t {
	case I8:
		return int64(int8(b[0])), nil
	case I16:
		return int64(int16(binary.BigEndian.Uint16(b))), nil
	case I32:
		return int64(int32(binary.BigEndian.Uint32(b))), nil
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

// Float reads an F32 or F64 field.
func (sc *Schema) Float(v []byte, name string) (float64, error) {
	b, t, err := sc.fixed(v, name, F32, F64)
	if err != nil {
		return 0, err
	}
	if t == F32 {
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
}

// Bool reads a Bool field.
func (sc *Schema) Bool(v []byte, name string) (bool, error) {
	b, _, err := sc.fixed(v, name, Bool)
	if err != nil {
		return false, err
	}
	if b[0] > 1 {
		return false, schemaErr("bool byte not 0 or 1")
	}
	return b[0] == 1, nil
}

// Bytes reads a Bytes field as a zero-copy slice of v.
func (sc *Schema) Bytes(v []byte, name string) ([]byte, error) {
	return sc.varField(v, name, Bytes)
}

// String reads a String field; the result aliases v (same validity window as v).
func (sc *Schema) String(v []byte, name string) (string, error) {
	b, err := sc.varField(v, name, String)
	if err != nil || len(b) == 0 {
		return "", err
	}
	return unsafe.String(unsafe.SliceData(b), len(b)), nil
}

func (sc *Schema) varField(v []byte, name string, want FieldType) ([]byte, error) {
	i, pos, err := sc.locate(v, name)
	if err != nil {
		return nil, err
	}
	if sc.fields[i].Type != want {
		return nil, schemaErr("field %q has another type", name)
	}
	if pos+2 > len(v) {
		return nil, schemaErr("value truncated")
	}
	n := int(binary.BigEndian.Uint16(v[pos:]))
	if pos+2+n > len(v) {
		return nil, schemaErr("value truncated")
	}
	return v[pos+2 : pos+2+n], nil
}

// Encode builds a value from vals in declaration order: uint64/int64/float64/
// bool/string/[]byte (any integer or float kind is accepted for numeric fields).
func (sc *Schema) Encode(vals ...any) ([]byte, error) {
	if len(vals) != len(sc.fields) {
		return nil, schemaErr("expected %d values, got %d", len(sc.fields), len(vals))
	}
	var out []byte
	for i, f := range sc.fields {
		x := vals[i]
		switch f.Type {
		case U8, U16, U32, U64:
			n, ok := toUint(x)
			if !ok || (f.Type != U64 && n >= 1<<(8*uint(fixedWidth[f.Type]))) {
				return nil, schemaErr("field %q: bad unsigned value %v", f.Name, x)
			}
			out = appendUint(out, n, fixedWidth[f.Type])
		case I8, I16, I32, I64:
			n, ok := toInt(x)
			w := uint(fixedWidth[f.Type])
			if !ok || (w < 8 && (n < -(1<<(8*w-1)) || n >= 1<<(8*w-1))) {
				return nil, schemaErr("field %q: bad signed value %v", f.Name, x)
			}
			out = appendUint(out, uint64(n), int(w))
		case F32, F64:
			fl, ok := toFloat(x)
			if !ok {
				return nil, schemaErr("field %q: bad float value %v", f.Name, x)
			}
			if f.Type == F32 {
				out = binary.BigEndian.AppendUint32(out, math.Float32bits(float32(fl)))
			} else {
				out = binary.BigEndian.AppendUint64(out, math.Float64bits(fl))
			}
		case Bool:
			b, ok := x.(bool)
			if !ok {
				return nil, schemaErr("field %q: bad bool value %v", f.Name, x)
			}
			out = append(out, map[bool]byte{false: 0, true: 1}[b])
		case String, Bytes:
			var b []byte
			switch t := x.(type) {
			case string:
				b = []byte(t)
			case []byte:
				b = t
			default:
				return nil, schemaErr("field %q: bad string/bytes value %v", f.Name, x)
			}
			if len(b) > math.MaxUint16 {
				return nil, schemaErr("field %q longer than 65535 bytes", f.Name)
			}
			out = binary.BigEndian.AppendUint16(out, uint16(len(b)))
			out = append(out, b...)
		}
	}
	return out, nil
}

func appendUint(b []byte, n uint64, w int) []byte {
	for i := w - 1; i >= 0; i-- {
		b = append(b, byte(n>>(8*uint(i))))
	}
	return b
}

func toUint(x any) (uint64, bool) {
	switch t := x.(type) {
	case uint8:
		return uint64(t), true
	case uint16:
		return uint64(t), true
	case uint32:
		return uint64(t), true
	case uint64:
		return t, true
	case uint:
		return uint64(t), true
	case int:
		return uint64(t), t >= 0
	case int64:
		return uint64(t), t >= 0
	}
	return 0, false
}

func toInt(x any) (int64, bool) {
	switch t := x.(type) {
	case int8:
		return int64(t), true
	case int16:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	}
	return 0, false
}

func toFloat(x any) (float64, bool) {
	switch t := x.(type) {
	case float32:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}
