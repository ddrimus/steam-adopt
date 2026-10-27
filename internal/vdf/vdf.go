// Binary VDF codec for shortcuts.vdf, round-tripping an unchanged file to the same bytes
package vdf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Type tags of the binary format
const (
	typeObject = 0x00
	typeString = 0x01
	typeInt32  = 0x02
	typeUint64 = 0x07
	typeEnd    = 0x08
)

// Error for when the data ends in the middle of a value
var ErrTruncated = errors.New("unexpected end of data")

// One ordered level of key/value pairs, values being *Object, string, int32 or uint64
type Object struct {
	keys []string
	vals map[string]any
}

// Create an empty Object
func NewObject() *Object {
	return &Object{vals: map[string]any{}}
}

// List the keys in file order
func (o *Object) Keys() []string {
	return o.keys
}

// Count the keys
func (o *Object) Len() int {
	return len(o.keys)
}

// Get the value under key
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Get the value under key when it is a string
func (o *Object) String(key string) (string, bool) {
	s, ok := o.vals[key].(string)
	return s, ok
}

// Store a value, keeping an existing key's position and appending a new one
func (o *Object) Set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// Parse binary VDF data, ignoring bytes after the top level's closing tag
func Unmarshal(data []byte) (*Object, error) {
	r := reader{data: data}
	return r.object(0)
}

// Nesting limit, so a corrupt file can't exhaust the stack
const maxDepth = 64

// Position in the data being parsed
type reader struct {
	data []byte
	pos  int
}

// Read one NUL-terminated string
func (r *reader) cstr() (string, error) {
	end := bytes.IndexByte(r.data[r.pos:], 0)
	if end < 0 {
		return "", ErrTruncated
	}
	s := string(r.data[r.pos : r.pos+end])
	r.pos += end + 1
	return s, nil
}

// Read the next n bytes
func (r *reader) take(n int) ([]byte, error) {
	if r.pos+n > len(r.data) {
		return nil, ErrTruncated
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

// Read key/value pairs until the tag that closes this level
func (r *reader) object(depth int) (*Object, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("nested deeper than %d levels at byte %d", maxDepth, r.pos)
	}
	o := NewObject()
	for {
		if r.pos >= len(r.data) {
			return nil, ErrTruncated
		}
		t := r.data[r.pos]
		r.pos++
		if t == typeEnd {
			return o, nil
		}
		key, err := r.cstr()
		if err != nil {
			return nil, err
		}
		switch t {
		case typeObject:
			sub, err := r.object(depth + 1)
			if err != nil {
				return nil, err
			}
			o.Set(key, sub)
		case typeString:
			s, err := r.cstr()
			if err != nil {
				return nil, err
			}
			o.Set(key, s)
		case typeInt32:
			b, err := r.take(4)
			if err != nil {
				return nil, err
			}
			o.Set(key, int32(binary.LittleEndian.Uint32(b)))
		case typeUint64:
			b, err := r.take(8)
			if err != nil {
				return nil, err
			}
			o.Set(key, binary.LittleEndian.Uint64(b))
		default:
			return nil, fmt.Errorf("unknown vdf type %#x at byte %d", t, r.pos-1)
		}
	}
}

// Serialize an Object back into binary VDF, writing values of any other type as strings
func Marshal(o *Object) []byte {
	var out bytes.Buffer
	marshal(&out, o)
	return out.Bytes()
}

// Write one level and its closing tag
func marshal(out *bytes.Buffer, o *Object) {
	for _, k := range o.keys {
		switch v := o.vals[k].(type) {
		case *Object:
			writeKey(out, typeObject, k)
			marshal(out, v)
		case int32:
			writeKey(out, typeInt32, k)
			out.Write(binary.LittleEndian.AppendUint32(nil, uint32(v)))
		case uint64:
			writeKey(out, typeUint64, k)
			out.Write(binary.LittleEndian.AppendUint64(nil, v))
		case string:
			writeKey(out, typeString, k)
			out.WriteString(v)
			out.WriteByte(0)
		default:
			writeKey(out, typeString, k)
			fmt.Fprint(out, v)
			out.WriteByte(0)
		}
	}
	out.WriteByte(typeEnd)
}

// Write a value's type tag and key
func writeKey(out *bytes.Buffer, t byte, key string) {
	out.WriteByte(t)
	out.WriteString(key)
	out.WriteByte(0)
}
