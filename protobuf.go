package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"math"
	"sort"
	"unicode/utf8"
)

type protobuf []byte

func (b protobuf) uint(field int, value uint64) protobuf {
	b = binary.AppendUvarint(b, uint64(field<<3))
	return binary.AppendUvarint(b, value)
}
func (b protobuf) bytes(field int, value []byte) protobuf {
	b = binary.AppendUvarint(b, uint64(field<<3|2))
	b = binary.AppendUvarint(b, uint64(len(value)))
	return append(b, value...)
}
func (b protobuf) text(field int, value string) protobuf { return b.bytes(field, []byte(value)) }
func (b protobuf) double(field int, value float64) protobuf {
	b = binary.AppendUvarint(b, uint64(field<<3|1))
	return binary.LittleEndian.AppendUint64(b, math.Float64bits(value))
}

type pbField struct {
	number, wire int
	n            uint64
	data         []byte
}

func pbFields(data []byte) ([]pbField, error) {
	out := []pbField{}
	for len(data) > 0 {
		key, n := binary.Uvarint(data)
		if n <= 0 || key>>3 == 0 || key>>3 > 0x1fffffff {
			return nil, failure(502, "Malformed Cursor protobuf tag")
		}
		data = data[n:]
		field := pbField{number: int(key >> 3), wire: int(key & 7)}
		switch field.wire {
		case 0:
			value, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, failure(502, "Malformed Cursor protobuf varint")
			}
			field.n = value
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			field.data = data[:8]
			data = data[8:]
		case 2:
			size, n := binary.Uvarint(data)
			if n <= 0 || size > uint64(len(data)-max(n, 0)) {
				return nil, io.ErrUnexpectedEOF
			}
			data = data[n:]
			field.data = data[:int(size)]
			data = data[int(size):]
		case 5:
			if len(data) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			field.data = data[:4]
			data = data[4:]
		default:
			return nil, failure(502, "Unsupported Cursor protobuf wire type %d", field.wire)
		}
		if len(out) >= 100000 {
			return nil, failure(502, "Cursor protobuf field limit exceeded")
		}
		out = append(out, field)
	}
	return out, nil
}
func pbGet(fields []pbField, num int) *pbField {
	for i := range fields {
		if fields[i].number == num {
			return &fields[i]
		}
	}
	return nil
}
func pbText(fields []pbField, num int) string {
	f := pbGet(fields, num)
	if f == nil {
		return ""
	}
	return string(f.data)
}
func pbNum(fields []pbField, num int) uint64 {
	f := pbGet(fields, num)
	if f == nil {
		return 0
	}
	return f.n
}
func pbValue(value any) protobuf {
	var out protobuf
	switch v := value.(type) {
	case nil:
		return out.uint(1, 0)
	case bool:
		if v {
			return out.uint(4, 1)
		}
		return out.uint(4, 0)
	case string:
		return out.text(3, v)
	case float64:
		return out.double(2, v)
	case int:
		return out.double(2, float64(v))
	case []any:
		var list protobuf
		for _, item := range v {
			list = list.bytes(1, pbValue(item))
		}
		return out.bytes(6, list)
	case map[string]any:
		var object protobuf
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			object = object.bytes(1, protobuf(nil).text(1, k).bytes(2, pbValue(v[k])))
		}
		return out.bytes(5, object)
	default:
		return out.uint(1, 0)
	}
}
func pbAny(data []byte) (any, error) { remaining := 100000; return readPBValue(data, 0, &remaining) }
func readPBValue(data []byte, depth int, remaining *int) (any, error) {
	if depth > 64 {
		return nil, failure(502, "Cursor tool argument nesting limit exceeded")
	}
	fields, err := pbFields(data)
	if err != nil {
		return nil, err
	}
	*remaining -= len(fields)
	if *remaining < 0 {
		return nil, failure(502, "Cursor tool argument field limit exceeded")
	}
	var value any
	for _, f := range fields {
		if f.number < 1 || f.number > 6 {
			continue
		}
		want := []int{0, 1, 2, 0, 2, 2}[f.number-1]
		if f.wire != want {
			return nil, failure(502, "Invalid Cursor tool argument wire type")
		}
		switch f.number {
		case 1:
			if f.n != 0 {
				return nil, failure(502, "Invalid protobuf NullValue")
			}
			value = nil
		case 2:
			v := math.Float64frombits(binary.LittleEndian.Uint64(f.data))
			if math.IsInf(v, 0) || math.IsNaN(v) {
				return nil, failure(502, "Nonfinite Cursor tool argument")
			}
			value = v
		case 3:
			if !utf8.Valid(f.data) {
				return nil, failure(502, "Invalid UTF-8 in Cursor tool arguments")
			}
			value = string(f.data)
		case 4:
			value = f.n != 0
		case 5:
			entries, err := pbFields(f.data)
			if err != nil {
				return nil, err
			}
			*remaining -= len(entries)
			if *remaining < 0 {
				return nil, failure(502, "Cursor tool argument field limit exceeded")
			}
			m := map[string]any{}
			for _, entry := range entries {
				if entry.number != 1 {
					continue
				}
				if entry.wire != 2 {
					return nil, failure(502, "Invalid Cursor argument map")
				}
				kv, err := pbFields(entry.data)
				if err != nil {
					return nil, err
				}
				*remaining -= len(kv)
				if *remaining < 0 {
					return nil, failure(502, "Cursor tool argument field limit exceeded")
				}
				key := ""
				var item any
				for _, part := range kv {
					switch part.number {
					case 1:
						if part.wire != 2 || !utf8.Valid(part.data) {
							return nil, failure(502, "Invalid Cursor argument key")
						}
						key = string(part.data)
					case 2:
						if part.wire != 2 {
							return nil, failure(502, "Invalid Cursor argument value")
						}
						item, err = readPBValue(part.data, depth+1, remaining)
						if err != nil {
							return nil, err
						}
					}
				}
				m[key] = item
			}
			value = m
		case 6:
			items, err := pbFields(f.data)
			if err != nil {
				return nil, err
			}
			*remaining -= len(items)
			if *remaining < 0 {
				return nil, failure(502, "Cursor tool argument field limit exceeded")
			}
			list := []any{}
			for _, item := range items {
				if item.number != 1 {
					continue
				}
				if item.wire != 2 {
					return nil, failure(502, "Invalid Cursor argument list")
				}
				v, err := readPBValue(item.data, depth+1, remaining)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			value = list
		}
	}
	return value, nil
}
func connectFrame(data []byte) []byte {
	out := make([]byte, 5, len(data)+5)
	binary.BigEndian.PutUint32(out[1:], uint32(len(data)))
	return append(out, data...)
}
func readConnectFrame(reader io.Reader) (data []byte, end bool, err error) {
	var head [5]byte
	if _, err = io.ReadFull(reader, head[:]); err != nil {
		return nil, false, err
	}
	size := binary.BigEndian.Uint32(head[1:])
	if size > maxWireBytes || head[0]&^byte(3) != 0 {
		return nil, false, failure(502, "Malformed Cursor Connect frame")
	}
	data = make([]byte, size)
	if _, err = io.ReadFull(reader, data); err != nil {
		return nil, false, err
	}
	if head[0]&1 != 0 {
		gz, e := gzip.NewReader(bytes.NewReader(data))
		if e != nil {
			return nil, false, e
		}
		data, e = io.ReadAll(io.LimitReader(gz, maxWireBytes+1))
		_ = gz.Close()
		if e != nil {
			return nil, false, e
		}
		if len(data) > maxWireBytes {
			return nil, false, failure(502, "Cursor decompressed frame exceeds limit")
		}
	}
	return data, head[0]&2 != 0, nil
}
