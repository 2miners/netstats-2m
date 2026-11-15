// Package kaspa is a minimal Kaspa gRPC client (protowire.RPC/MessageStream
// over plaintext HTTP/2) for the few calls the reporter needs, with protobuf
// written by hand instead of generated code.
package kaspa

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field int, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendBytes(b []byte, field int, v []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

func appendString(b []byte, field int, s string) []byte { return appendBytes(b, field, []byte(s)) }

func appendUint(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, wireVarint)
	return appendVarint(b, v)
}

// msg is a decoded protobuf message: field number -> raw values in order
// (varints as uint64, fixed64 as uint64 bits, bytes as []byte).
type msg map[int][]any

var errBadPB = errors.New("kaspa: malformed protobuf")

func decode(b []byte) (msg, error) {
	m := msg{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errBadPB
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errBadPB
			}
			b = b[n:]
			m[field] = append(m[field], v)
		case wireFixed64:
			if len(b) < 8 {
				return nil, errBadPB
			}
			m[field] = append(m[field], binary.LittleEndian.Uint64(b))
			b = b[8:]
		case wireFixed32:
			if len(b) < 4 {
				return nil, errBadPB
			}
			m[field] = append(m[field], uint64(binary.LittleEndian.Uint32(b)))
			b = b[4:]
		case wireBytes:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errBadPB
			}
			m[field] = append(m[field], b[n:n+int(l)])
			b = b[n+int(l):]
		default:
			return nil, errBadPB
		}
	}
	return m, nil
}

func (m msg) uint(field int) uint64 {
	if v, ok := m.first(field).(uint64); ok {
		return v
	}
	return 0
}

func (m msg) double(field int) float64 { return math.Float64frombits(m.uint(field)) }

func (m msg) bool(field int) bool { return m.uint(field) != 0 }

func (m msg) str(field int) string {
	if v, ok := m.first(field).([]byte); ok {
		return string(v)
	}
	return ""
}

func (m msg) strs(field int) []string {
	var out []string
	for _, v := range m[field] {
		if b, ok := v.([]byte); ok {
			out = append(out, string(b))
		}
	}
	return out
}

func (m msg) sub(field int) msg {
	b, ok := m.first(field).([]byte)
	if !ok {
		return msg{}
	}
	s, err := decode(b)
	if err != nil {
		return msg{}
	}
	return s
}

func (m msg) count(field int) int { return len(m[field]) }

func (m msg) first(field int) any {
	if vs := m[field]; len(vs) > 0 {
		return vs[0]
	}
	return nil
}
