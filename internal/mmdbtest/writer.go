// Package mmdbtest writes MaxMind DB files, the format of the databases that
// geo and asn read, for the tests: a small writer of the format, and
// databases with the schemas of GeoIP2 City and Country and of GeoLite2 ASN,
// filled with example data. Only tests import it.
//
// The format is described at https://maxmind.github.io/MaxMind-DB/: a binary
// search tree over the bits of the addresses, whose records lead to another
// node, to no data, or to a record of the data section; a 16-byte separator;
// the data section; and the metadata, after a marker.
package mmdbtest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// The types of the data section.
const (
	typePointer = 1
	typeString  = 2
	typeDouble  = 3
	typeBytes   = 4
	typeUint16  = 5
	typeUint32  = 6
	typeMap     = 7
	typeInt32   = 8
	typeUint64  = 9
	typeArray   = 11
	typeBool    = 14
	typeFloat   = 15
)

// metadataMarker starts the metadata, at the end of the file.
const metadataMarker = "\xAB\xCD\xEFMaxMind.com"

// separatorSize is the size of the separator between the search tree and
// the data section.
const separatorSize = 16

// Raw is data written as it is, unchecked: a record or a value that a test
// wants invalid.
type Raw []byte

// Writer builds a database in memory. The zero value is not ready: use New.
// A Writer is not safe for concurrent use.
type Writer struct {
	// IPVersion is 6, the default, for a tree of IPv6 addresses that holds
	// the IPv4 addresses at ::/96, or 4 for a tree of IPv4 addresses only.
	IPVersion int
	// RecordSize is the size of a record of the search tree in bits: 24, 28
	// or 32. 0 selects the smallest that the database needs.
	RecordSize int
	// DatabaseType, Languages, Description and BuildEpoch are written in the
	// metadata.
	DatabaseType string
	Languages    []string
	Description  map[string]string
	BuildEpoch   uint64
	// Pointers makes a string written again a pointer to its first copy,
	// as the databases of MaxMind do.
	Pointers bool
	// Metadata replaces or adds keys of the metadata.
	Metadata map[string]any

	nodes   []node
	data    bytes.Buffer
	records map[string]uint32 // the offset of each record, by its encoding
	strings map[string]uint32 // the offset of each string, with Pointers
}

// record is a record of the search tree: empty, the index of a node, or the
// offset of a record of the data section.
type record struct {
	kind  uint8
	value uint32
}

const (
	empty uint8 = iota
	toNode
	toData
)

type node struct{ records [2]record }

// New returns a Writer of a database of the type dbType, whose build time
// is buildEpoch, in seconds since the Unix epoch.
func New(dbType string, buildEpoch uint64) *Writer {
	return &Writer{
		IPVersion:    6,
		DatabaseType: dbType,
		Languages:    []string{"en"},
		Description:  map[string]string{"en": "tessera test database"},
		BuildEpoch:   buildEpoch,
		Pointers:     true,
		nodes:        []node{{}},
		records:      map[string]uint32{},
		strings:      map[string]uint32{},
	}
}

// Insert maps every address of network to data. A network inserted later
// replaces the data of the earlier ones for its addresses, those of the
// networks it contains included. data is a value of the data section:
//
//   - map[string]any, a map;
//   - []any, an array;
//   - string, a UTF-8 string;
//   - bool, a boolean;
//   - float64, a double, and float32, a float;
//   - uint16, uint32, uint64 and int32, integers of these types;
//   - []byte, bytes;
//   - Raw, bytes written as they are;
//   - Ref, a record that Add wrote to the data section of w.
func (w *Writer) Insert(network netip.Prefix, data any) error {
	if !network.IsValid() || network.Bits() == 0 {
		return errors.New("mmdbtest: a network of at least one bit is required")
	}
	network = network.Masked()
	addr, bits := network.Addr(), network.Bits()
	var key [16]byte
	switch {
	case w.IPVersion == 4 && addr.Is6():
		return errors.New("mmdbtest: an IPv6 network in an IPv4 database")
	case w.IPVersion == 4:
		a := addr.As4()
		copy(key[:], a[:])
	case addr.Is4():
		a := addr.As4()
		copy(key[12:], a[:])
		bits += 96
	default:
		key = addr.As16()
	}
	offset, err := w.offset(data)
	if err != nil {
		return err
	}
	leaf := record{kind: toData, value: offset}
	n := uint32(0)
	for i := range bits {
		bit := key[i/8] >> (7 - i%8) & 1
		if i == bits-1 {
			w.nodes[n].records[bit] = leaf
			break
		}
		r := w.nodes[n].records[bit]
		switch r.kind {
		case toNode:
			n = r.value
			continue
		case empty:
			w.nodes = append(w.nodes, node{})
		case toData:
			// A larger network holds this one: its data stays on both
			// sides of the new node.
			w.nodes = append(w.nodes, node{records: [2]record{r, r}})
		}
		next := uint32(len(w.nodes) - 1)
		w.nodes[n].records[bit] = record{kind: toNode, value: next}
		n = next
	}
	return nil
}

// Ref is a record of the data section of a Writer, which Add returns.
type Ref struct{ offset uint32 }

// Add writes data to the data section, as Insert does, and returns a Ref
// to it: Inserting the Ref maps a network to the record without encoding it
// again, as a large database of fewer records than networks needs.
func (w *Writer) Add(data any) (Ref, error) {
	offset, err := w.store(data)
	return Ref{offset}, err
}

// offset returns the offset of data in the data section: that of a Ref, or
// that of the record that store writes.
func (w *Writer) offset(data any) (uint32, error) {
	if r, ok := data.(Ref); ok {
		return r.offset, nil
	}
	return w.store(data)
}

// Encode returns v encoded as a value of the data section, without
// pointers: Inserting the Raw it returns writes the same record, without
// encoding it again, as a large database built from a few records needs.
func Encode(v any) (Raw, error) {
	var e encoder
	if err := e.value(v); err != nil {
		return nil, err
	}
	return Raw(e.buf.Bytes()), nil
}

// store writes data to the data section, once for each distinct record, and
// returns its offset.
func (w *Writer) store(data any) (uint32, error) {
	var plain encoder
	if err := plain.value(data); err != nil {
		return 0, err
	}
	key := plain.buf.String()
	if offset, ok := w.records[key]; ok {
		return offset, nil
	}
	offset := uint32(w.data.Len())
	if !w.Pointers {
		w.data.Write(plain.buf.Bytes())
	} else {
		e := encoder{strings: w.strings, base: offset}
		if err := e.value(data); err != nil {
			return 0, err
		}
		w.data.Write(e.buf.Bytes())
	}
	w.records[key] = offset
	return offset, nil
}

// Bytes returns the database.
func (w *Writer) Bytes() ([]byte, error) {
	count := uint64(len(w.nodes))
	largest := count + separatorSize + uint64(w.data.Len())
	size := w.RecordSize
	switch {
	case size == 0 && largest < 1<<24:
		size = 24
	case size == 0 && largest < 1<<28:
		size = 28
	case size == 0:
		size = 32
	}
	switch {
	case size != 24 && size != 28 && size != 32:
		return nil, fmt.Errorf("mmdbtest: record size %d", size)
	case largest >= 1<<size:
		return nil, fmt.Errorf("mmdbtest: the database needs records of more than %d bits", size)
	}
	var out bytes.Buffer
	out.Grow(len(w.nodes)*size/4 + separatorSize + w.data.Len() + 512)
	value := func(r record) uint32 {
		switch r.kind {
		case toNode:
			return r.value
		case toData:
			return uint32(count) + separatorSize + r.value
		}
		return uint32(count)
	}
	var b [8]byte
	for _, n := range w.nodes {
		left, right := value(n.records[0]), value(n.records[1])
		switch size {
		case 24:
			b = [8]byte{byte(left >> 16), byte(left >> 8), byte(left), byte(right >> 16), byte(right >> 8), byte(right)}
			out.Write(b[:6])
		case 28:
			b = [8]byte{byte(left >> 16), byte(left >> 8), byte(left), byte(left>>24)<<4 | byte(right>>24)&0x0f, byte(right >> 16), byte(right >> 8), byte(right)}
			out.Write(b[:7])
		case 32:
			binary.BigEndian.PutUint32(b[:4], left)
			binary.BigEndian.PutUint32(b[4:], right)
			out.Write(b[:])
		}
	}
	out.Write(make([]byte, separatorSize))
	out.Write(w.data.Bytes())
	out.WriteString(metadataMarker)
	languages := make([]any, len(w.Languages))
	for i, l := range w.Languages {
		languages[i] = l
	}
	description := map[string]any{}
	for k, v := range w.Description {
		description[k] = v
	}
	metadata := map[string]any{
		"binary_format_major_version": uint16(2),
		"binary_format_minor_version": uint16(0),
		"build_epoch":                 w.BuildEpoch,
		"database_type":               w.DatabaseType,
		"description":                 description,
		"ip_version":                  uint16(w.IPVersion),
		"languages":                   languages,
		"node_count":                  uint32(count),
		"record_size":                 uint16(size),
	}
	maps.Copy(metadata, w.Metadata)
	var meta encoder
	if err := meta.value(metadata); err != nil {
		return nil, err
	}
	out.Write(meta.buf.Bytes())
	return out.Bytes(), nil
}

// WriteFile writes the database to path.
func (w *Writer) WriteFile(path string) error {
	data, err := w.Bytes()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// encoder encodes values of the data section. With strings, it writes a
// pointer to a string already written, and records the strings it writes at
// base plus their offset in buf.
type encoder struct {
	buf     bytes.Buffer
	strings map[string]uint32
	base    uint32
}

func (e *encoder) value(v any) error {
	switch v := v.(type) {
	case map[string]any:
		e.control(typeMap, len(v))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			e.string(k)
			if err := e.value(v[k]); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	case []any:
		e.control(typeArray, len(v))
		for i, item := range v {
			if err := e.value(item); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	case string:
		e.string(v)
	case bool:
		size := 0
		if v {
			size = 1
		}
		e.control(typeBool, size)
	case float64:
		e.control(typeDouble, 8)
		e.uint(math.Float64bits(v), 8)
	case float32:
		e.control(typeFloat, 4)
		e.uint(uint64(math.Float32bits(v)), 4)
	case uint16:
		e.unsigned(typeUint16, uint64(v))
	case uint32:
		e.unsigned(typeUint32, uint64(v))
	case uint64:
		e.unsigned(typeUint64, v)
	case int32:
		e.unsigned(typeInt32, uint64(uint32(v)))
	case []byte:
		e.control(typeBytes, len(v))
		e.buf.Write(v)
	case Raw:
		e.buf.Write(v)
	default:
		return fmt.Errorf("mmdbtest: no type of the data section for %T", v)
	}
	return nil
}

// string writes s, or a pointer to the copy of s already written.
func (e *encoder) string(s string) {
	if e.strings != nil {
		if offset, ok := e.strings[s]; ok {
			e.pointer(offset)
			return
		}
		// A pointer is as long as a short string: only longer ones are
		// worth one.
		if len(s) >= 4 {
			e.strings[s] = e.base + uint32(e.buf.Len())
		}
	}
	e.control(typeString, len(s))
	e.buf.WriteString(s)
}

// unsigned writes n in as few bytes as it needs.
func (e *encoder) unsigned(typ int, n uint64) {
	size := 0
	for x := n; x > 0; x >>= 8 {
		size++
	}
	e.control(typ, size)
	e.uint(n, size)
}

// uint writes the size low bytes of n, big-endian.
func (e *encoder) uint(n uint64, size int) {
	for i := size - 1; i >= 0; i-- {
		e.buf.WriteByte(byte(n >> (8 * i)))
	}
}

// control writes the control byte of a value of the type typ and of the
// size size, then the byte of an extended type and the bytes of a large
// size.
func (e *encoder) control(typ, size int) {
	var first byte
	if typ <= 7 {
		first = byte(typ) << 5
	}
	switch {
	case size < 29:
		first |= byte(size)
	case size < 285:
		first |= 29
	case size < 65821:
		first |= 30
	default:
		first |= 31
	}
	e.buf.WriteByte(first)
	if typ > 7 {
		e.buf.WriteByte(byte(typ - 7))
	}
	switch {
	case size < 29:
	case size < 285:
		e.uint(uint64(size-29), 1)
	case size < 65821:
		e.uint(uint64(size-285), 2)
	default:
		e.uint(uint64(size-65821), 3)
	}
}

// pointer writes a pointer to the offset p of the data section.
func (e *encoder) pointer(p uint32) {
	switch {
	case p < 2048:
		e.buf.WriteByte(typePointer<<5 | byte(p>>8)&7)
		e.uint(uint64(p), 1)
	case p < 526336:
		q := p - 2048
		e.buf.WriteByte(typePointer<<5 | 1<<3 | byte(q>>16)&7)
		e.uint(uint64(q), 2)
	case p < 134744064:
		q := p - 526336
		e.buf.WriteByte(typePointer<<5 | 2<<3 | byte(q>>24)&7)
		e.uint(uint64(q), 3)
	default:
		e.buf.WriteByte(typePointer<<5 | 3<<3)
		e.uint(uint64(p), 4)
	}
}

// names returns the names of a place in several languages, as GeoIP2 writes
// them: name in English, and a translation that tessera does not read.
func names(name string) map[string]any {
	return map[string]any{"en": name, "de": name, "ja": strings.ToUpper(name)}
}
