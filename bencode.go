package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Bencode

type bKind uint8

const (
	kindInt bKind = iota
	kindBytes
	kindList
	kindDict
)

type bValue struct {
	kind bKind
	i    int64
	b    []byte
	l    []*bValue
	d    *bDict
}

type bDict struct {
	keys [][]byte
	vals map[string]*bValue
}

func newBDict() *bDict {
	return &bDict{vals: make(map[string]*bValue)}
}

func newInt(i int64) *bValue           { return &bValue{kind: kindInt, i: i} }
func newBytes(b []byte) *bValue        { return &bValue{kind: kindBytes, b: b} }
func newString(s string) *bValue       { return newBytes([]byte(s)) }
func newList(items ...*bValue) *bValue { return &bValue{kind: kindList, l: items} }
func newDictValue() *bValue            { return &bValue{kind: kindDict, d: newBDict()} }

func (d *bDict) get(key string) *bValue {
	if d == nil {
		return nil
	}
	return d.vals[key]
}

func (d *bDict) getBytes(key []byte) *bValue {
	return d.get(string(key))
}

func (d *bDict) set(key []byte, v *bValue) {
	if d == nil {
		return
	}
	k := string(key)
	if _, ok := d.vals[k]; !ok {
		d.keys = append(d.keys, append([]byte(nil), key...))
	}
	d.vals[k] = v
}

func (d *bDict) setString(key string, v *bValue) {
	d.set([]byte(key), v)
}

func (d *bDict) del(key string) bool {
	if d == nil {
		return false
	}
	if _, ok := d.vals[key]; !ok {
		return false
	}
	delete(d.vals, key)
	for i, k := range d.keys {
		if string(k) == key {
			d.keys = append(d.keys[:i], d.keys[i+1:]...)
			break
		}
	}
	return true
}

func (d *bDict) length() int {
	if d == nil {
		return 0
	}
	return len(d.keys)
}

func (d *bDict) sortedKeys() [][]byte {
	if d == nil {
		return nil
	}
	keys := make([][]byte, len(d.keys))
	for i, k := range d.keys {
		keys[i] = append([]byte(nil), k...)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	return keys
}

type bParser struct {
	data []byte
	pos  int
}

func parseBencode(data []byte) (*bValue, error) {
	p := &bParser{data: data}
	v, err := p.parse(0)
	if err != nil {
		return nil, err
	}
	for p.pos < len(p.data) && isBencodeSpace(p.data[p.pos]) {
		p.pos++
	}
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("trailing data at byte %d", p.pos)
	}
	return v, nil
}

func isBencodeSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}

func (p *bParser) parse(depth int) (*bValue, error) {
	if depth > 512 {
		return nil, fmt.Errorf("bencode nesting is too deep at byte %d", p.pos)
	}
	if p.pos >= len(p.data) {
		return nil, errors.New("unexpected end of bencode data")
	}
	switch c := p.data[p.pos]; {
	case c == 'i':
		p.pos++
		start := p.pos
		for p.pos < len(p.data) && p.data[p.pos] != 'e' {
			p.pos++
		}
		if p.pos >= len(p.data) {
			return nil, fmt.Errorf("unterminated integer at byte %d", start-1)
		}
		s := string(p.data[start:p.pos])
		p.pos++
		if s == "" || s == "-" {
			return nil, fmt.Errorf("invalid integer at byte %d", start-1)
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q at byte %d: %w", s, start-1, err)
		}
		return newInt(i), nil

	case c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.data) && p.data[p.pos] != ':' {
			if p.data[p.pos] < '0' || p.data[p.pos] > '9' {
				return nil, fmt.Errorf("invalid string length at byte %d", start)
			}
			p.pos++
		}
		if p.pos >= len(p.data) {
			return nil, fmt.Errorf("unterminated string length at byte %d", start)
		}
		n, err := strconv.ParseInt(string(p.data[start:p.pos]), 10, 64)
		p.pos++ // ':'
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid string length at byte %d", start)
		}
		if n > int64(len(p.data)-p.pos) {
			return nil, fmt.Errorf("string at byte %d is longer than the remaining data", start)
		}
		b := p.data[p.pos : p.pos+int(n)]
		p.pos += int(n)
		return newBytes(b), nil

	case c == 'l':
		p.pos++
		list := &bValue{kind: kindList}
		for {
			if p.pos >= len(p.data) {
				return nil, errors.New("unterminated list")
			}
			if p.data[p.pos] == 'e' {
				p.pos++
				return list, nil
			}
			v, err := p.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			list.l = append(list.l, v)
		}

	case c == 'd':
		p.pos++
		dict := newDictValue()
		for {
			if p.pos >= len(p.data) {
				return nil, errors.New("unterminated dictionary")
			}
			if p.data[p.pos] == 'e' {
				p.pos++
				return dict, nil
			}
			key, err := p.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			if key.kind != kindBytes {
				return nil, fmt.Errorf("dictionary key is not a string at byte %d", p.pos)
			}
			val, err := p.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			dict.d.set(key.b, val)
		}

	case c == 'e':
		return nil, fmt.Errorf("unexpected 'e' at byte %d", p.pos)
	default:
		return nil, fmt.Errorf("invalid bencode token %q at byte %d", c, p.pos)
	}
}

// findRawInfo returns the exact bytes of the top-level "info" value.  Keeping
// these bytes lets us edit the rest of a torrent without ever re-encoding info
// and therefore without changing either v1 or v2 info hashes.
func findRawInfo(data []byte) ([]byte, error) {
	p := &bParser{data: data}
	if p.pos >= len(p.data) || p.data[p.pos] != 'd' {
		return nil, errors.New("metainfo is not a bencoded dictionary")
	}
	p.pos++
	var raw []byte
	for {
		if p.pos >= len(p.data) {
			return nil, errors.New("unterminated top-level dictionary")
		}
		if p.data[p.pos] == 'e' {
			p.pos++
			break
		}
		key, err := p.parse(0)
		if err != nil {
			return nil, err
		}
		if key.kind != kindBytes {
			return nil, errors.New("dictionary key is not a string")
		}
		start := p.pos
		if _, err := p.parse(0); err != nil {
			return nil, err
		}
		if string(key.b) == "info" {
			raw = append([]byte(nil), data[start:p.pos]...)
		}
	}
	return raw, nil
}

func encodeBencode(v *bValue) []byte {
	var buf bytes.Buffer
	writeBValue(&buf, v)
	return buf.Bytes()
}

func writeBValue(buf *bytes.Buffer, v *bValue) {
	if v == nil {
		return
	}
	switch v.kind {
	case kindInt:
		buf.WriteByte('i')
		buf.WriteString(strconv.FormatInt(v.i, 10))
		buf.WriteByte('e')
	case kindBytes:
		buf.WriteString(strconv.Itoa(len(v.b)))
		buf.WriteByte(':')
		buf.Write(v.b)
	case kindList:
		buf.WriteByte('l')
		for _, item := range v.l {
			writeBValue(buf, item)
		}
		buf.WriteByte('e')
	case kindDict:
		buf.WriteByte('d')
		for _, key := range v.d.sortedKeys() {
			writeBValue(buf, newBytes(key))
			writeBValue(buf, v.d.get(string(key)))
		}
		buf.WriteByte('e')
	}
}

// encodeTop encodes a top-level metainfo dictionary.  If rawInfo is available
// and info has not been modified, the original raw info bytes are spliced back
// in verbatim.
func encodeTop(top *bValue, rawInfo []byte, infoDirty bool) []byte {
	if top == nil || top.kind != kindDict {
		return nil
	}
	if !infoDirty && len(rawInfo) > 0 && top.d.get("info") != nil {
		var buf bytes.Buffer
		buf.WriteByte('d')
		for _, key := range top.d.sortedKeys() {
			writeBValue(&buf, newBytes(key))
			if string(key) == "info" {
				buf.Write(rawInfo)
			} else {
				writeBValue(&buf, top.d.get(string(key)))
			}
		}
		buf.WriteByte('e')
		return buf.Bytes()
	}
	return encodeBencode(top)
}

func bEqual(a, b *bValue) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case kindInt:
		return a.i == b.i
	case kindBytes:
		return bytes.Equal(a.b, b.b)
	case kindList:
		if len(a.l) != len(b.l) {
			return false
		}
		for i := range a.l {
			if !bEqual(a.l[i], b.l[i]) {
				return false
			}
		}
		return true
	case kindDict:
		if a.d.length() != b.d.length() {
			return false
		}
		for _, k := range a.d.keys {
			bv := b.d.get(string(k))
			if bv == nil || !bEqual(a.d.get(string(k)), bv) {
				return false
			}
		}
		return true
	}
	return false
}

func isPrintableUTF8(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

const b64KeyPrefix = "$b64:"

func jsonKeyName(key []byte) string {
	if utf8.Valid(key) {
		return string(key)
	}
	return b64KeyPrefix + base64.StdEncoding.EncodeToString(key)
}

func bValueToJSON(v *bValue) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case kindInt:
		return v.i
	case kindBytes:
		if isPrintableUTF8(v.b) {
			return string(v.b)
		}
		return map[string]any{"$base64": base64.StdEncoding.EncodeToString(v.b)}
	case kindList:
		out := make([]any, len(v.l))
		for i, item := range v.l {
			out[i] = bValueToJSON(item)
		}
		return out
	case kindDict:
		out := make(map[string]any, v.d.length())
		for _, key := range v.d.keys {
			out[jsonKeyName(key)] = bValueToJSON(v.d.get(string(key)))
		}
		return out
	}
	return nil
}

func jsonToBValue(v any) (*bValue, error) {
	switch x := v.(type) {
	case nil:
		return nil, errors.New("null cannot be represented in bencode")
	case bool:
		return nil, errors.New("booleans cannot be represented in bencode; use 1 or 0")
	case string:
		return newBytes([]byte(x)), nil
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return nil, fmt.Errorf("non-integer number %q cannot be represented in bencode", s)
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer %q is out of range: %w", s, err)
		}
		return newInt(i), nil
	case float64:
		if x != float64(int64(x)) {
			return nil, fmt.Errorf("non-integer number %v cannot be represented in bencode", x)
		}
		return newInt(int64(x)), nil
	case []any:
		out := &bValue{kind: kindList, l: make([]*bValue, 0, len(x))}
		for _, item := range x {
			itemVal, err := jsonToBValue(item)
			if err != nil {
				return nil, err
			}
			out.l = append(out.l, itemVal)
		}
		return out, nil
	case map[string]any:
		if len(x) == 1 {
			if s, ok := x["$base64"].(string); ok {
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, fmt.Errorf("invalid $base64 value: %w", err)
				}
				return newBytes(b), nil
			}
			if s, ok := x["$int"].(string); ok {
				i, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("invalid $int value: %w", err)
				}
				return newInt(i), nil
			}
		}
		out := newDictValue()
		for key, raw := range x {
			keyBytes := []byte(key)
			if strings.HasPrefix(key, b64KeyPrefix) {
				b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(key, b64KeyPrefix))
				if err != nil {
					return nil, fmt.Errorf("invalid base64 dictionary key %q: %w", key, err)
				}
				keyBytes = b
			}
			itemVal, err := jsonToBValue(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			out.d.set(keyBytes, itemVal)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value of type %T", v)
	}
}

func parseJSONValue(data []byte) (*bValue, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("more than one JSON value")
		}
		return nil, err
	}
	return jsonToBValue(x)
}

func formatBValue(v *bValue, max int) string {
	var sb strings.Builder
	formatBValueInto(&sb, v, max, 0)
	return sb.String()
}

func formatBValueInto(sb *strings.Builder, v *bValue, max, depth int) {
	if v == nil {
		sb.WriteString("<nil>")
		return
	}
	if sb.Len() >= max {
		return
	}
	switch v.kind {
	case kindInt:
		sb.WriteString(strconv.FormatInt(v.i, 10))
	case kindBytes:
		if isPrintableUTF8(v.b) {
			sb.WriteString(strconv.Quote(string(v.b)))
		} else {
			enc := base64.StdEncoding.EncodeToString(v.b)
			if len(enc) > 48 {
				enc = enc[:48] + "..."
			}
			fmt.Fprintf(sb, "<%d bytes, base64:%s>", len(v.b), enc)
		}
	case kindList:
		if depth >= 4 {
			fmt.Fprintf(sb, "<list of %d>", len(v.l))
			return
		}
		sb.WriteByte('[')
		for i, item := range v.l {
			if sb.Len() >= max {
				sb.WriteString("...")
				break
			}
			if i > 0 {
				sb.WriteString(", ")
			}
			formatBValueInto(sb, item, max, depth+1)
		}
		sb.WriteByte(']')
	case kindDict:
		if depth >= 4 {
			fmt.Fprintf(sb, "<dict of %d>", v.d.length())
			return
		}
		sb.WriteByte('{')
		first := true
		for _, key := range v.d.keys {
			if sb.Len() >= max {
				sb.WriteString("...")
				break
			}
			if !first {
				sb.WriteString(", ")
			}
			first = false
			sb.WriteString(strconv.Quote(string(key)))
			sb.WriteString(": ")
			formatBValueInto(sb, v.d.get(string(key)), max, depth+1)
		}
		sb.WriteByte('}')
	}
}
