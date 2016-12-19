package tsdb

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/fabxc/tsdb/chunks"
)

// SeriesReader provides reading access of serialized time series data.
type SeriesReader interface {
	// Chunk returns the series data chunk with the given reference.
	Chunk(ref uint32) (chunks.Chunk, error)
}

// seriesReader implements a SeriesReader for a serialized byte stream
// of series data.
type seriesReader struct {
	// The underlying byte slice holding the encoded series data.
	b []byte
}

func newSeriesReader(b []byte) (*seriesReader, error) {
	// Verify magic number.
	if m := binary.BigEndian.Uint32(b[:4]); m != MagicSeries {
		return nil, fmt.Errorf("invalid magic number %x", m)
	}
	return &seriesReader{b: b}, nil
}

func (s *seriesReader) Chunk(offset uint32) (chunks.Chunk, error) {
	b := s.b[offset:]

	l, n := binary.Uvarint(b)
	if n < 0 {
		return nil, fmt.Errorf("reading chunk length failed")
	}
	b = b[n:]
	enc := chunks.Encoding(b[0])

	c, err := chunks.FromData(enc, b[1:1+l])
	if err != nil {
		return nil, err
	}
	return c, nil
}

// IndexReader provides reading access of serialized index data.
type IndexReader interface {
	// Stats returns statisitics about the indexed data.
	Stats() (BlockStats, error)

	// LabelValues returns the possible label values
	LabelValues(names ...string) (StringTuples, error)

	// Postings returns the postings list iterator for the label pair.
	Postings(name, value string) (Postings, error)

	// Series returns the series for the given reference.
	Series(ref uint32, mint, maxt int64) (Series, error)
}

// StringTuples provides access to a sorted list of string tuples.
type StringTuples interface {
	// Total number of tuples in the list.
	Len() int
	// At returns the tuple at position i.
	At(i int) ([]string, error)
}

type indexReader struct {
	series SeriesReader

	// The underlying byte slice holding the encoded series data.
	b []byte

	// Cached hashmaps of section offsets.
	labels   map[string]uint32
	postings map[string]uint32
}

var (
	errInvalidSize = fmt.Errorf("invalid size")
	errInvalidFlag = fmt.Errorf("invalid flag")
	errNotFound    = fmt.Errorf("not found")
)

func newIndexReader(s SeriesReader, b []byte) (*indexReader, error) {
	if len(b) < 16 {
		return nil, errInvalidSize
	}
	r := &indexReader{
		series: s,
		b:      b,
	}

	// Verify magic number.
	if m := binary.BigEndian.Uint32(b[:4]); m != MagicIndex {
		return nil, fmt.Errorf("invalid magic number %x", m)
	}

	var err error
	// The last two 4 bytes hold the pointers to the hashmaps.
	loff := binary.BigEndian.Uint32(b[len(b)-8 : len(b)-4])
	poff := binary.BigEndian.Uint32(b[len(b)-4:])

	if r.labels, err = readHashmap(r.section(loff)); err != nil {
		return nil, err
	}
	if r.postings, err = readHashmap(r.section(poff)); err != nil {
		return nil, err
	}

	return r, nil
}

func readHashmap(flag byte, b []byte, err error) (map[string]uint32, error) {
	if err != nil {
		return nil, err
	}
	if flag != flagStd {
		return nil, errInvalidFlag
	}
	h := make(map[string]uint32, 512)

	for len(b) > 0 {
		l, n := binary.Uvarint(b)
		if n < 1 {
			return nil, errInvalidSize
		}
		b = b[n:]

		if len(b) < int(l) {
			return nil, errInvalidSize
		}
		s := string(b[:l])
		b = b[l:]

		o, n := binary.Uvarint(b)
		if n < 1 {
			return nil, errInvalidSize
		}
		b = b[n:]

		h[s] = uint32(o)
	}

	return h, nil
}

func (r *indexReader) section(o uint32) (byte, []byte, error) {
	b := r.b[o:]

	if len(b) < 5 {
		return 0, nil, errInvalidSize
	}

	flag := b[0]
	l := binary.BigEndian.Uint32(b[1:5])

	b = b[5:]

	// b must have the given length plus 4 bytes for the CRC32 checksum.
	if len(b) < int(l)+4 {
		return 0, nil, errInvalidSize
	}
	return flag, b[:l], nil
}

func (r *indexReader) lookupSymbol(o uint32) ([]byte, error) {
	l, n := binary.Uvarint(r.b[o:])
	if n < 0 {
		return nil, fmt.Errorf("reading symbol length failed")
	}

	end := int(o) + n + int(l)
	if end > len(r.b) {
		return nil, fmt.Errorf("invalid length")
	}

	return r.b[int(o)+n : end], nil
}

func (r *indexReader) Stats() (BlockStats, error) {
	flag, b, err := r.section(8)
	if err != nil {
		return BlockStats{}, err
	}
	if flag != flagStd {
		return BlockStats{}, errInvalidFlag
	}

	if len(b) != 64 {
		return BlockStats{}, errInvalidSize
	}

	return BlockStats{
		MinTime:     int64(binary.BigEndian.Uint64(b)),
		MaxTime:     int64(binary.BigEndian.Uint64(b[8:])),
		SeriesCount: binary.BigEndian.Uint32(b[16:]),
		ChunkCount:  binary.BigEndian.Uint32(b[20:]),
		SampleCount: binary.BigEndian.Uint64(b[24:]),
	}, nil
}

func (r *indexReader) LabelValues(names ...string) (StringTuples, error) {
	key := strings.Join(names, string(sep))
	off, ok := r.labels[key]
	if !ok {
		return nil, fmt.Errorf("label index doesn't exist")
	}

	flag, b, err := r.section(off)
	if err != nil {
		return nil, fmt.Errorf("section: %s", err)
	}
	if flag != flagStd {
		return nil, errInvalidFlag
	}
	l, n := binary.Uvarint(b)
	if n < 1 {
		return nil, errInvalidSize
	}

	st := &serializedStringTuples{
		l:      int(l),
		b:      b[n:],
		lookup: r.lookupSymbol,
	}
	return st, nil
}

func (r *indexReader) Series(ref uint32, mint, maxt int64) (Series, error) {
	k, n := binary.Uvarint(r.b[ref:])
	if n < 1 {
		return nil, errInvalidSize
	}

	b := r.b[int(ref)+n:]
	offsets := make([]uint32, 0, k)

	for i := 0; i < int(k); i++ {
		o, n := binary.Uvarint(b)
		if n < 1 {
			return nil, errInvalidSize
		}
		offsets = append(offsets, uint32(o))

		b = b[n:]
	}
	// Symbol offests must occur in pairs representing name and value.
	if len(offsets)&1 != 0 {
		return nil, errInvalidSize
	}

	// TODO(fabxc): Fully materialize series symbols for now. Figure out later if it
	// makes sense to decode those lazily.
	// If we use unsafe strings the there'll be no copy overhead.
	//
	// The references are expected to be sorted and match the order of
	// the underlying strings.
	labels := make(Labels, 0, k)

	for i := 0; i < int(k); i += 2 {
		n, err := r.lookupSymbol(offsets[i])
		if err != nil {
			return nil, err
		}
		v, err := r.lookupSymbol(offsets[i+1])
		if err != nil {
			return nil, err
		}
		labels = append(labels, Label{
			Name:  string(n),
			Value: string(v),
		})
	}

	// Read the chunks meta data.
	k, n = binary.Uvarint(b)
	if n < 1 {
		return nil, errInvalidSize
	}

	b = b[n:]
	chunks := make([]ChunkMeta, 0, k)

	for i := 0; i < int(k); i++ {
		firstTime, n := binary.Varint(b)
		if n < 1 {
			return nil, errInvalidSize
		}
		b = b[n:]

		// Terminate early if we exceeded the queried time range.
		if firstTime > maxt {
			break
		}

		lastTime, n := binary.Varint(b)
		if n < 1 {
			return nil, errInvalidSize
		}
		b = b[n:]

		o, n := binary.Uvarint(b)
		if n < 1 {
			return nil, errInvalidSize
		}
		b = b[n:]

		// Skip the chunk if it is before the queried time range.
		if lastTime < mint {
			continue
		}

		chunks = append(chunks, ChunkMeta{
			Ref:     uint32(o),
			MinTime: firstTime,
			MaxTime: lastTime,
		})
	}
	// If no chunks applicable to the time range were found, the series
	// can be skipped.
	if len(chunks) == 0 {
		return nil, nil
	}

	return &series{
		labels: labels,
		chunks: chunks,
		chunk:  r.series.Chunk,
	}, nil
}

func (r *indexReader) Postings(name, value string) (Postings, error) {
	key := name + string(sep) + value

	off, ok := r.postings[key]
	if !ok {
		return nil, errNotFound
	}

	flag, b, err := r.section(off)
	if err != nil {
		return nil, err
	}

	if flag != flagStd {
		return nil, errInvalidFlag
	}

	// TODO(fabxc): just read into memory as an intermediate solution.
	// Add iterator over serialized data.
	var l []uint32

	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errInvalidSize
		}
		l = append(l, binary.BigEndian.Uint32(b[:4]))

		b = b[4:]
	}

	return &listPostings{list: l, idx: -1}, nil
}

type stringTuples struct {
	l int      // tuple length
	s []string // flattened tuple entries
}

func newStringTuples(s []string, l int) (*stringTuples, error) {
	if len(s)%l != 0 {
		return nil, errInvalidSize
	}
	return &stringTuples{s: s, l: l}, nil
}

func (t *stringTuples) Len() int                   { return len(t.s) / t.l }
func (t *stringTuples) At(i int) ([]string, error) { return t.s[i : i+t.l], nil }

func (t *stringTuples) Swap(i, j int) {
	c := make([]string, t.l)
	copy(c, t.s[i:i+t.l])

	for k := 0; k < t.l; k++ {
		t.s[i+k] = t.s[j+k]
		t.s[j+k] = c[k]
	}
}

func (t *stringTuples) Less(i, j int) bool {
	for k := 0; k < t.l; k++ {
		d := strings.Compare(t.s[i+k], t.s[j+k])

		if d < 0 {
			return true
		}
		if d > 0 {
			return false
		}
	}
	return false
}

type serializedStringTuples struct {
	l      int
	b      []byte
	lookup func(uint32) ([]byte, error)
}

func (t *serializedStringTuples) Len() int {
	// TODO(fabxc): Cache this?
	return len(t.b) / (4 * t.l)
}

func (t *serializedStringTuples) At(i int) ([]string, error) {
	if len(t.b) < (i+t.l)*4 {
		return nil, errInvalidSize
	}
	res := make([]string, t.l)

	for k := 0; k < t.l; k++ {
		offset := binary.BigEndian.Uint32(t.b[i*4:])

		b, err := t.lookup(offset)
		if err != nil {
			return nil, fmt.Errorf("lookup: %s", err)
		}
		res = append(res, string(b))
	}

	return res, nil
}