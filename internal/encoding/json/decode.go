// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/golang/protobuf/v2/internal/errors"
)

// call specifies which Decoder method was invoked.
type call uint8

const (
	readCall call = iota
	peekCall
)

// Decoder is a token-based JSON decoder.
type Decoder struct {
	// lastCall is last method called, either readCall or peekCall.
	// Initial value is readCall.
	lastCall call

	// value contains the last read value.
	value Value

	// err contains the last read error.
	err error

	// startStack is a stack containing StartObject and StartArray types. The
	// top of stack represents the object or the array the current value is
	// directly located in.
	startStack []Type

	// orig is used in reporting line and column.
	orig []byte
	// in contains the unconsumed input.
	in []byte
}

// NewDecoder returns a Decoder to read the given []byte.
func NewDecoder(b []byte) *Decoder {
	return &Decoder{orig: b, in: b}
}

// Peek looks ahead and returns the next JSON type without advancing a read.
func (d *Decoder) Peek() Type {
	defer func() { d.lastCall = peekCall }()
	if d.lastCall == readCall {
		d.value, d.err = d.Read()
	}
	return d.value.typ
}

// Read returns the next JSON value. It will return an error if there is no
// valid value.  For String types containing invalid UTF8 characters, a
// non-fatal error is returned and caller can call Read for the next value.
func (d *Decoder) Read() (Value, error) {
	defer func() { d.lastCall = readCall }()
	if d.lastCall == peekCall {
		return d.value, d.err
	}

	var nerr errors.NonFatal
	value, n, err := d.parseNext()
	if !nerr.Merge(err) {
		return Value{}, err
	}

	switch value.typ {
	case EOF:
		if len(d.startStack) != 0 ||
			d.value.typ&Null|Bool|Number|String|EndObject|EndArray == 0 {
			return Value{}, io.ErrUnexpectedEOF
		}

	case Null:
		if !d.isValueNext() {
			return Value{}, d.newSyntaxError("unexpected value null")
		}

	case Bool, Number:
		if !d.isValueNext() {
			return Value{}, d.newSyntaxError("unexpected value %v", value.Raw())
		}

	case String:
		if d.isValueNext() {
			break
		}
		// Check if this is for an object name.
		if d.value.typ&(StartObject|comma) == 0 {
			return Value{}, d.newSyntaxError("unexpected value %v", value.Raw())
		}
		d.in = d.in[n:]
		d.consume(0)
		if c := d.in[0]; c != ':' {
			return Value{}, d.newSyntaxError(`unexpected character %v, missing ":" after object name`, string(c))
		}
		n = 1
		value.typ = Name

	case StartObject, StartArray:
		if !d.isValueNext() {
			return Value{}, d.newSyntaxError("unexpected character %v", value.Raw())
		}
		d.startStack = append(d.startStack, value.typ)

	case EndObject:
		if len(d.startStack) == 0 ||
			d.value.typ == comma ||
			d.startStack[len(d.startStack)-1] != StartObject {
			return Value{}, d.newSyntaxError("unexpected character }")
		}
		d.startStack = d.startStack[:len(d.startStack)-1]

	case EndArray:
		if len(d.startStack) == 0 ||
			d.value.typ == comma ||
			d.startStack[len(d.startStack)-1] != StartArray {
			return Value{}, d.newSyntaxError("unexpected character ]")
		}
		d.startStack = d.startStack[:len(d.startStack)-1]

	case comma:
		if len(d.startStack) == 0 ||
			d.value.typ&(Null|Bool|Number|String|EndObject|EndArray) == 0 {
			return Value{}, d.newSyntaxError("unexpected character ,")
		}
	}

	// Update lastType only after validating value to be in the right
	// sequence.
	d.value.typ = value.typ
	d.in = d.in[n:]

	if d.value.typ == comma {
		return d.Read()
	}
	return value, nerr.E
}

var (
	literalRegexp = regexp.MustCompile(`^(null|true|false)`)
	// Any sequence that looks like a non-delimiter (for error reporting).
	errRegexp = regexp.MustCompile(`^([-+._a-zA-Z0-9]{1,32}|.)`)
)

// parseNext parses for the next JSON value. It returns a Value object for
// different types, except for Name. It also returns the size that was parsed.
// It does not handle whether the next value is in a valid sequence or not, it
// only ensures that the value is a valid one.
func (d *Decoder) parseNext() (value Value, n int, err error) {
	// Trim leading spaces.
	d.consume(0)

	in := d.in
	if len(in) == 0 {
		return d.newValue(nil, EOF), 0, nil
	}

	switch in[0] {
	case 'n', 't', 'f':
		n := matchWithDelim(literalRegexp, in)
		if n == 0 {
			return Value{}, 0, d.newSyntaxError("invalid value %s", errRegexp.Find(in))
		}
		switch in[0] {
		case 'n':
			return d.newValue(in[:n], Null), n, nil
		case 't':
			return d.newBoolValue(in[:n], true), n, nil
		case 'f':
			return d.newBoolValue(in[:n], false), n, nil
		}

	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		n, ok := consumeNumber(in)
		if !ok {
			return Value{}, 0, d.newSyntaxError("invalid number %s", errRegexp.Find(in))
		}
		return d.newValue(in[:n], Number), n, nil

	case '"':
		var nerr errors.NonFatal
		s, n, err := d.parseString(in)
		if !nerr.Merge(err) {
			return Value{}, 0, err
		}
		return d.newStringValue(in[:n], s), n, nerr.E

	case '{':
		return d.newValue(in[:1], StartObject), 1, nil

	case '}':
		return d.newValue(in[:1], EndObject), 1, nil

	case '[':
		return d.newValue(in[:1], StartArray), 1, nil

	case ']':
		return d.newValue(in[:1], EndArray), 1, nil

	case ',':
		return d.newValue(in[:1], comma), 1, nil
	}
	return Value{}, 0, d.newSyntaxError("invalid value %s", errRegexp.Find(in))
}

// position returns line and column number of parsed bytes.
func (d *Decoder) position() (int, int) {
	// Calculate line and column of consumed input.
	b := d.orig[:len(d.orig)-len(d.in)]
	line := bytes.Count(b, []byte("\n")) + 1
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[i+1:]
	}
	column := utf8.RuneCount(b) + 1 // ignore multi-rune characters
	return line, column
}

// newSyntaxError returns an error with line and column information useful for
// syntax errors.
func (d *Decoder) newSyntaxError(f string, x ...interface{}) error {
	e := errors.New(f, x...)
	line, column := d.position()
	return errors.New("syntax error (line %d:%d): %v", line, column, e)
}

// matchWithDelim matches r with the input b and verifies that the match
// terminates with a delimiter of some form (e.g., r"[^-+_.a-zA-Z0-9]").
// As a special case, EOF is considered a delimiter.
func matchWithDelim(r *regexp.Regexp, b []byte) int {
	n := len(r.Find(b))
	if n < len(b) {
		// Check that the next character is a delimiter.
		if isNotDelim(b[n]) {
			return 0
		}
	}
	return n
}

// isNotDelim returns true if given byte is a not delimiter character.
func isNotDelim(c byte) bool {
	return (c == '-' || c == '+' || c == '.' || c == '_' ||
		('a' <= c && c <= 'z') ||
		('A' <= c && c <= 'Z') ||
		('0' <= c && c <= '9'))
}

// consume consumes n bytes of input and any subsequent whitespace.
func (d *Decoder) consume(n int) {
	d.in = d.in[n:]
	for len(d.in) > 0 {
		switch d.in[0] {
		case ' ', '\n', '\r', '\t':
			d.in = d.in[1:]
		default:
			return
		}
	}
}

// isValueNext returns true if next type should be a JSON value: Null,
// Number, String or Bool.
func (d *Decoder) isValueNext() bool {
	if len(d.startStack) == 0 {
		return d.value.typ == 0
	}

	start := d.startStack[len(d.startStack)-1]
	switch start {
	case StartObject:
		return d.value.typ&Name != 0
	case StartArray:
		return d.value.typ&(StartArray|comma) != 0
	}
	panic(fmt.Sprintf(
		"unreachable logic in Decoder.isValueNext, lastType: %v, startStack: %v",
		d.value.typ, start))
}

// newValue constructs a Value for given Type.
func (d *Decoder) newValue(input []byte, typ Type) Value {
	line, column := d.position()
	return Value{
		input:  input,
		line:   line,
		column: column,
		typ:    typ,
	}
}

// newBoolValue constructs a Value for a JSON boolean.
func (d *Decoder) newBoolValue(input []byte, b bool) Value {
	line, column := d.position()
	return Value{
		input:  input,
		line:   line,
		column: column,
		typ:    Bool,
		boo:    b,
	}
}

// newStringValue constructs a Value for a JSON string.
func (d *Decoder) newStringValue(input []byte, s string) Value {
	line, column := d.position()
	return Value{
		input:  input,
		line:   line,
		column: column,
		typ:    String,
		str:    s,
	}
}

// Clone returns a copy of the Decoder for use in reading ahead the next JSON
// object, array or other values without affecting current Decoder.
func (d *Decoder) Clone() *Decoder {
	ret := *d
	ret.startStack = append([]Type(nil), ret.startStack...)
	return &ret
}

// Value contains a JSON type and value parsed from calling Decoder.Read.
// For JSON boolean and string, it holds the converted value in boo and str
// fields respectively. For JSON number, input field holds a valid number which
// is converted only in Int or Float. Other JSON types do not require any
// additional data.
type Value struct {
	input  []byte
	line   int
	column int
	typ    Type
	boo    bool
	str    string
}

func (v Value) newError(f string, x ...interface{}) error {
	e := errors.New(f, x...)
	return errors.New("error (line %d:%d): %v", v.line, v.column, e)
}

// Type returns the JSON type.
func (v Value) Type() Type {
	return v.typ
}

// Position returns the line and column of the value.
func (v Value) Position() (int, int) {
	return v.line, v.column
}

// Bool returns the bool value if token is Bool, else it will return an error.
func (v Value) Bool() (bool, error) {
	if v.typ != Bool {
		return false, v.newError("%s is not a bool", v.input)
	}
	return v.boo, nil
}

// String returns the string value for a JSON string token or the read value in
// string if token is not a string.
func (v Value) String() string {
	if v.typ != String {
		return string(v.input)
	}
	return v.str
}

// Name returns the object name if token is Name, else it will return an error.
func (v Value) Name() (string, error) {
	if v.typ != Name {
		return "", v.newError("%s is not an object name", v.input)
	}
	return v.str, nil
}

// Raw returns the read value in string.
func (v Value) Raw() string {
	return string(v.input)
}

// Float returns the floating-point number if token is Number, else it will
// return an error.
//
// The floating-point precision is specified by the bitSize parameter: 32 for
// float32 or 64 for float64. If bitSize=32, the result still has type float64,
// but it will be convertible to float32 without changing its value. It will
// return an error if the number exceeds the floating point limits for given
// bitSize.
func (v Value) Float(bitSize int) (float64, error) {
	if v.typ != Number {
		return 0, v.newError("%s is not a number", v.input)
	}
	f, err := strconv.ParseFloat(string(v.input), bitSize)
	if err != nil {
		return 0, v.newError("%v", err)
	}
	return f, nil
}

// Int returns the signed integer number if token is Number, else it will
// return an error.
//
// The given bitSize specifies the integer type that the result must fit into.
// It returns an error if the number is not an integer value or if the result
// exceeds the limits for given bitSize.
func (v Value) Int(bitSize int) (int64, error) {
	s, err := v.getIntStr()
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, bitSize)
	if err != nil {
		return 0, v.newError("%v", err)
	}
	return n, nil
}

// Uint returns the signed integer number if token is Number, else it will
// return an error.
//
// The given bitSize specifies the unsigned integer type that the result must
// fit into.  It returns an error if the number is not an unsigned integer value
// or if the result exceeds the limits for given bitSize.
func (v Value) Uint(bitSize int) (uint64, error) {
	s, err := v.getIntStr()
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(s, 10, bitSize)
	if err != nil {
		return 0, v.newError("%v", err)
	}
	return n, nil
}

func (v Value) getIntStr() (string, error) {
	if v.typ != Number {
		return "", v.newError("%s is not a number", v.input)
	}
	parts, ok := parseNumber(v.input)
	if !ok {
		return "", v.newError("%s is not a number", v.input)
	}
	num, ok := normalizeToIntString(parts)
	if !ok {
		return "", v.newError("cannot convert %s to integer", v.input)
	}
	return num, nil
}
