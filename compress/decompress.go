/*
   Copyright 2022 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compress

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon-lib/mmap"
)

type codeword struct {
	len     byte          // Number of bits in the codes
	pattern *word         // Pattern corresponding to entries
	ptr     *patternTable // pointer to deeper level tables
}

type patternTable struct {
	bitLen   int // Number of bits to lookup in the table
	patterns []*codeword
}

type posTable struct {
	bitLen int // Number of bits to lookup in the table
	pos    []uint64
	lens   []byte
	ptrs   []*posTable
}

// Decompressor provides access to the superstrings in a file produced by a compressor
type Decompressor struct {
	compressedFile string
	f              *os.File
	mmapHandle1    []byte                 // mmap handle for unix (this is used to close mmap)
	mmapHandle2    *[mmap.MaxMapSize]byte // mmap handle for windows (this is used to close mmap)
	data           []byte                 // slice of correct size for the decompressor to work with
	dict           *patternTable
	posDict        *posTable
	wordsStart     uint64 // Offset of whether the superstrings actually start
	size           int64

	wordsCount, emptyWordsCount uint64
}

func NewDecompressor(compressedFile string) (*Decompressor, error) {
	d := &Decompressor{
		compressedFile: compressedFile,
	}

	var err error
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("decompressing file: %s, %+v, trace: %s", compressedFile, rec, dbg.Stack())
		}
	}()

	d.f, err = os.Open(compressedFile)
	if err != nil {
		return nil, err
	}
	var stat os.FileInfo
	if stat, err = d.f.Stat(); err != nil {
		return nil, err
	}
	d.size = stat.Size()
	if d.size < 32 {
		return nil, fmt.Errorf("compressed file is too short: %d", d.size)
	}
	if d.mmapHandle1, d.mmapHandle2, err = mmap.Mmap(d.f, int(d.size)); err != nil {
		return nil, err
	}

	// read patterns from file
	d.data = d.mmapHandle1[:d.size]
	d.wordsCount = binary.BigEndian.Uint64(d.data[:8])
	d.emptyWordsCount = binary.BigEndian.Uint64(d.data[8:16])
	dictSize := binary.BigEndian.Uint64(d.data[16:24])
	data := d.data[24 : 24+dictSize]
	var depths []uint64
	var patterns [][]byte
	var i uint64
	var patternMaxDepth uint64

	//fmt.Printf("[decomp] dictSize = %d\n", dictSize)
	for i < dictSize {
		d, ns := binary.Uvarint(data[i:])
		depths = append(depths, d)
		if d > patternMaxDepth {
			patternMaxDepth = d
		}
		i += uint64(ns)
		l, n := binary.Uvarint(data[i:])
		i += uint64(n)
		patterns = append(patterns, data[i:i+l])
		//fmt.Printf("depth = %d, pattern = [%x]\n", d, data[i:i+l])
		i += l
	}

	if dictSize > 0 {
		var bitLen int
		if patternMaxDepth > 9 {
			bitLen = 9
		} else {
			bitLen = int(patternMaxDepth)
		}
		//fmt.Printf("pattern maxDepth=%d\n", tree.maxDepth)
		tableSize := 1 << bitLen
		d.dict = &patternTable{
			bitLen:   bitLen,
			patterns: make([]*codeword, tableSize),
		}
		if _, err := buildPatternTable(d.dict, depths, patterns, 0, 0, 0, patternMaxDepth); err != nil {
			return nil, err
		}
	}

	// read positions
	pos := 24 + dictSize
	dictSize = binary.BigEndian.Uint64(d.data[pos : pos+8])
	data = d.data[pos+8 : pos+8+dictSize]
	var posDepths []uint64
	var poss []uint64
	var posMaxDepth uint64
	//fmt.Printf("[decomp] posDictSize = %d\n", dictSize)
	i = 0
	for i < dictSize {
		d, ns := binary.Uvarint(data[i:])
		posDepths = append(posDepths, d)
		if d > posMaxDepth {
			posMaxDepth = d
		}
		i += uint64(ns)
		pos, n := binary.Uvarint(data[i:])
		i += uint64(n)
		poss = append(poss, pos)
	}

	if dictSize > 0 {
		var bitLen int
		if posMaxDepth > 9 {
			bitLen = 9
		} else {
			bitLen = int(posMaxDepth)
		}
		//fmt.Printf("pos maxDepth=%d\n", tree.maxDepth)
		tableSize := 1 << bitLen
		d.posDict = &posTable{
			bitLen: bitLen,
			pos:    make([]uint64, tableSize),
			lens:   make([]byte, tableSize),
			ptrs:   make([]*posTable, tableSize),
		}
		buildPosTable(posDepths, poss, d.posDict, 0, 0, 0, posMaxDepth)
	}
	d.wordsStart = pos + 8 + dictSize
	return d, nil
}

type word []byte // plain text word associated with code from dictionary

// returns number of depth and patterns comsumed
func buildPatternTable(table *patternTable, depths []uint64, patterns [][]byte, code uint16, bits int, depth uint64, maxDepth uint64) (int, error) {
	if len(depths) == 0 {
		return 0, nil
	}
	if depth == depths[0] {
		pattern := word(patterns[0])
		//fmt.Printf("depth=%d, maxDepth=%d, code=[%b], codeLen=%d, pattern=[%x]\n", depth, maxDepth, code, bits, pattern)

		codeStep := uint16(1) << bits
		codeFrom, codeTo := code, code+codeStep
		if table.bitLen != bits {
			codeTo = code | (uint16(1) << table.bitLen)
		}

		cw := &codeword{pattern: &pattern, len: byte(bits), ptr: nil}
		for c := codeFrom; c < codeTo; c += codeStep {
			if p := table.patterns[c]; p == nil {
				table.patterns[c] = cw
			} else {
				p.pattern, p.len, p.ptr = &pattern, byte(bits), nil
			}
		}
		return 1, nil
	}
	if bits == 9 {
		var bitLen int
		if maxDepth > 9 {
			bitLen = 9
		} else {
			bitLen = int(maxDepth)
		}
		tableSize := 1 << bitLen
		newTable := &patternTable{
			bitLen:   bitLen,
			patterns: make([]*codeword, tableSize),
		}

		table.patterns[code] = &codeword{pattern: nil, len: byte(0), ptr: newTable}
		return buildPatternTable(newTable, depths, patterns, 0, 0, depth, maxDepth)
	}
	if maxDepth == 0 {
		return 0, fmt.Errorf("invalid snapshot format. decompress.buildPatternTable faced maxDepth underflow")
	}
	b0, err := buildPatternTable(table, depths, patterns, code, bits+1, depth+1, maxDepth-1)
	if err != nil {
		return 0, err
	}
	b1, err := buildPatternTable(table, depths[b0:], patterns[b0:], (uint16(1)<<bits)|code, bits+1, depth+1, maxDepth-1)
	if err != nil {
		return 0, err
	}
	return b0 + b1, nil
}

func buildPosTable(depths []uint64, poss []uint64, table *posTable, code uint16, bits int, depth uint64, maxDepth uint64) int {
	if len(depths) == 0 {
		return 0
	}
	if depth == depths[0] {
		p := poss[0]
		//fmt.Printf("depth=%d, maxDepth=%d, code=[%b], codeLen=%d, pos=%d\n", depth, maxDepth, code, bits, p)
		if table.bitLen == bits {
			table.pos[code] = p
			table.lens[code] = byte(bits)
			table.ptrs[code] = nil
		} else {
			codeStep := uint16(1) << bits
			codeFrom := code
			codeTo := code | (uint16(1) << table.bitLen)
			for c := codeFrom; c < codeTo; c += codeStep {
				table.pos[c] = p
				table.lens[c] = byte(bits)
				table.ptrs[c] = nil
			}
		}
		return 1
	}
	if bits == 9 {
		var bitLen int
		if maxDepth > 9 {
			bitLen = 9
		} else {
			bitLen = int(maxDepth)
		}
		tableSize := 1 << bitLen
		newTable := &posTable{
			bitLen: bitLen,
			pos:    make([]uint64, tableSize),
			lens:   make([]byte, tableSize),
			ptrs:   make([]*posTable, tableSize),
		}
		table.pos[code] = 0
		table.lens[code] = byte(0)
		table.ptrs[code] = newTable
		return buildPosTable(depths, poss, newTable, 0, 0, depth, maxDepth)
	}
	b0 := buildPosTable(depths, poss, table, code, bits+1, depth+1, maxDepth-1)
	return b0 + buildPosTable(depths[b0:], poss[b0:], table, (uint16(1)<<bits)|code, bits+1, depth+1, maxDepth-1)
}

func (d *Decompressor) Size() int64 {
	return d.size
}

func (d *Decompressor) Close() error {
	if err := mmap.Munmap(d.mmapHandle1, d.mmapHandle2); err != nil {
		return err
	}
	if err := d.f.Close(); err != nil {
		return err
	}
	return nil
}

func (d *Decompressor) FilePath() string { return d.compressedFile }

// WithReadAhead - Expect read in sequential order. (Hence, pages in the given range can be aggressively read ahead, and may be freed soon after they are accessed.)
func (d *Decompressor) WithReadAhead(f func() error) error {
	_ = mmap.MadviseSequential(d.mmapHandle1)
	defer mmap.MadviseRandom(d.mmapHandle1)
	return f()
}

// Getter represent "reader" or "interator" that can move accross the data of the decompressor
// The full state of the getter can be captured by saving dataP, and dataBit
type Getter struct {
	data        []byte
	dataP       uint64
	dataBit     int // Value 0..7 - position of the bit
	patternDict *patternTable
	posDict     *posTable
	fName       string
	trace       bool
}

func (g *Getter) Trace(t bool) { g.trace = t }

func (g *Getter) nextPos(clean bool) uint64 {
	if clean {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
	}
	table := g.posDict
	if table.bitLen == 0 {
		return table.pos[0]
	}
	var l byte
	var pos uint64
	for l == 0 {
		code := uint16(g.data[g.dataP]) >> g.dataBit
		if 8-g.dataBit < table.bitLen && int(g.dataP)+1 < len(g.data) {
			code |= uint16(g.data[g.dataP+1]) << (8 - g.dataBit)
		}
		code &= (uint16(1) << table.bitLen) - 1
		l = table.lens[code]
		if l == 0 {
			table = table.ptrs[code]
			g.dataBit += 9
		} else {
			g.dataBit += int(l)
			pos = table.pos[code]
		}
		g.dataP += uint64(g.dataBit / 8)
		g.dataBit = g.dataBit % 8
	}
	return pos
}

func (g *Getter) nextPattern() []byte {
	table := g.patternDict
	if table.bitLen == 0 {
		return *table.patterns[0].pattern
	}
	var l byte
	var pattern []byte
	for l == 0 {
		code := uint16(g.data[g.dataP]) >> g.dataBit
		if 8-g.dataBit < table.bitLen && int(g.dataP)+1 < len(g.data) {
			code |= uint16(g.data[g.dataP+1]) << (8 - g.dataBit)
		}
		code &= (uint16(1) << table.bitLen) - 1
		cw := table.patterns[code]
		l = cw.len
		if l == 0 {
			table = cw.ptr
			g.dataBit += 9
		} else {
			g.dataBit += int(l)
			pattern = *cw.pattern
		}
		g.dataP += uint64(g.dataBit / 8)
		g.dataBit = g.dataBit % 8
	}
	return pattern
}

func (g *Getter) Size() int {
	return len(g.data)
}

func (d *Decompressor) Count() int           { return int(d.wordsCount) }
func (d *Decompressor) EmptyWordsCount() int { return int(d.emptyWordsCount) }

// MakeGetter creates an object that can be used to access superstrings in the decompressor's file
// Getter is not thread-safe, but there can be multiple getters used simultaneously and concurrently
// for the same decompressor
func (d *Decompressor) MakeGetter() *Getter {
	return &Getter{patternDict: d.dict, posDict: d.posDict, data: d.data[d.wordsStart:], fName: d.compressedFile}
}

func (g *Getter) Reset(offset uint64) {
	g.dataP = offset
	g.dataBit = 0
}

func (g *Getter) HasNext() bool {
	return g.dataP < uint64(len(g.data))
}

// Next extracts a compressed word from current offset in the file
// and appends it to the given buf, returning the result of appending
// After extracting next word, it moves to the beginning of the next one
func (g *Getter) Next(buf []byte) ([]byte, uint64) {
	savePos := g.dataP
	wordLen := g.nextPos(true)
	wordLen-- // because when create huffman tree we do ++ , because 0 is terminator
	if wordLen == 0 {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
		return buf, g.dataP
	}
	bufPos := len(buf) // Tracking position in buf where to insert part of the word
	lastUncovered := len(buf)
	if len(buf)+int(wordLen) > cap(buf) {
		newBuf := make([]byte, len(buf)+int(wordLen))
		copy(newBuf, buf)
		buf = newBuf
	} else {
		// Expand buffer
		buf = buf[:len(buf)+int(wordLen)]
	}
	// Loop below fills in the patterns
	for pos := g.nextPos(false /* clean */); pos != 0; pos = g.nextPos(false) {
		bufPos += int(pos) - 1 // Positions where to insert patterns are encoded relative to one another
		copy(buf[bufPos:], g.nextPattern())
	}
	if g.dataBit > 0 {
		g.dataP++
		g.dataBit = 0
	}
	postLoopPos := g.dataP
	g.dataP = savePos
	g.dataBit = 0
	g.nextPos(true /* clean */) // Reset the state of huffman reader
	bufPos = lastUncovered      // Restore to the beginning of buf
	// Loop below fills the data which is not in the patterns
	for pos := g.nextPos(false); pos != 0; pos = g.nextPos(false) {
		bufPos += int(pos) - 1 // Positions where to insert patterns are encoded relative to one another
		if bufPos > lastUncovered {
			dif := uint64(bufPos - lastUncovered)
			copy(buf[lastUncovered:bufPos], g.data[postLoopPos:postLoopPos+dif])
			postLoopPos += dif
		}
		lastUncovered = bufPos + len(g.nextPattern())
	}
	if int(wordLen) > lastUncovered {
		dif := wordLen - uint64(lastUncovered)
		copy(buf[lastUncovered:wordLen], g.data[postLoopPos:postLoopPos+dif])
		postLoopPos += dif
	}
	g.dataP = postLoopPos
	g.dataBit = 0
	return buf, postLoopPos
}

func (g *Getter) NextUncompressed() ([]byte, uint64) {
	wordLen := g.nextPos(true)
	wordLen-- // because when create huffman tree we do ++ , because 0 is terminator
	if wordLen == 0 {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
		return g.data[g.dataP:g.dataP], g.dataP
	}
	g.nextPos(false)
	if g.dataBit > 0 {
		g.dataP++
		g.dataBit = 0
	}
	pos := g.dataP
	g.dataP += wordLen
	return g.data[pos:g.dataP], g.dataP
}

// Skip moves offset to the next word and returns the new offset.
func (g *Getter) Skip() uint64 {
	l := g.nextPos(true)
	l-- // because when create huffman tree we do ++ , because 0 is terminator
	if l == 0 {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
		return g.dataP
	}
	wordLen := int(l)

	var add uint64
	var bufPos int
	var lastUncovered int
	for pos := g.nextPos(false /* clean */); pos != 0; pos = g.nextPos(false) {
		bufPos += int(pos) - 1
		if wordLen < bufPos {
			panic(fmt.Sprintf("likely .idx is invalid: %s", g.fName))
		}
		if bufPos > lastUncovered {
			add += uint64(bufPos - lastUncovered)
		}
		lastUncovered = bufPos + len(g.nextPattern())
	}
	if g.dataBit > 0 {
		g.dataP++
		g.dataBit = 0
	}
	if int(l) > lastUncovered {
		add += l - uint64(lastUncovered)
	}
	// Uncovered characters
	g.dataP += add
	return g.dataP
}

func (g *Getter) SkipUncompressed() uint64 {
	wordLen := g.nextPos(true)
	wordLen-- // because when create huffman tree we do ++ , because 0 is terminator
	if wordLen == 0 {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
		return g.dataP
	}
	g.nextPos(false)
	if g.dataBit > 0 {
		g.dataP++
		g.dataBit = 0
	}
	g.dataP += wordLen
	return g.dataP
}

// Match returns true and next offset if the word at current offset fully matches the buf
// returns false and current offset otherwise.
func (g *Getter) Match(buf []byte) (bool, uint64) {
	savePos := g.dataP
	wordLen := g.nextPos(true)
	wordLen-- // because when create huffman tree we do ++ , because 0 is terminator
	lenBuf := len(buf)
	if wordLen == 0 || int(wordLen) != lenBuf {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
		if lenBuf != 0 {
			g.dataP, g.dataBit = savePos, 0
		}
		return lenBuf == int(wordLen), g.dataP
	}

	var bufPos int
	// In the first pass, we only check patterns
	for pos := g.nextPos(false /* clean */); pos != 0; pos = g.nextPos(false) {
		bufPos += int(pos) - 1
		pattern := g.nextPattern()
		if lenBuf < bufPos+len(pattern) || !bytes.Equal(buf[bufPos:bufPos+len(pattern)], pattern) {
			g.dataP, g.dataBit = savePos, 0
			return false, savePos
		}
	}
	if g.dataBit > 0 {
		g.dataP++
		g.dataBit = 0
	}
	postLoopPos := g.dataP
	g.dataP, g.dataBit = savePos, 0
	g.nextPos(true /* clean */) // Reset the state of huffman decoder
	// Second pass - we check spaces not covered by the patterns
	var lastUncovered int
	bufPos = 0
	for pos := g.nextPos(false /* clean */); pos != 0; pos = g.nextPos(false) {
		bufPos += int(pos) - 1
		if bufPos > lastUncovered {
			dif := uint64(bufPos - lastUncovered)
			if lenBuf < bufPos || !bytes.Equal(buf[lastUncovered:bufPos], g.data[postLoopPos:postLoopPos+dif]) {
				g.dataP, g.dataBit = savePos, 0
				return false, savePos
			}
			postLoopPos += dif
		}
		lastUncovered = bufPos + len(g.nextPattern())
	}
	if int(wordLen) > lastUncovered {
		dif := wordLen - uint64(lastUncovered)
		if lenBuf < int(wordLen) || !bytes.Equal(buf[lastUncovered:wordLen], g.data[postLoopPos:postLoopPos+dif]) {
			g.dataP, g.dataBit = savePos, 0
			return false, savePos
		}
		postLoopPos += dif
	}
	if lenBuf != int(wordLen) {
		g.dataP, g.dataBit = savePos, 0
		return false, savePos
	}
	g.dataP, g.dataBit = postLoopPos, 0
	return true, postLoopPos
}

// MatchPrefix only checks if the word at the current offset has a buf prefix. Does not move offset to the next word.
func (g *Getter) MatchPrefix(prefix []byte) bool {
	savePos := g.dataP
	defer func() {
		g.dataP, g.dataBit = savePos, 0
	}()

	wordLen := g.nextPos(true /* clean */)
	wordLen-- // because when create huffman tree we do ++ , because 0 is terminator
	prefixLen := len(prefix)
	if wordLen == 0 || int(wordLen) < prefixLen {
		if g.dataBit > 0 {
			g.dataP++
			g.dataBit = 0
		}
		if prefixLen != 0 {
			g.dataP, g.dataBit = savePos, 0
		}
		return prefixLen == int(wordLen)
	}

	var bufPos int
	// In the first pass, we only check patterns
	// Only run this loop as far as the prefix goes, there is no need to check further
	for pos := g.nextPos(false /* clean */); pos != 0; pos = g.nextPos(false) {
		bufPos += int(pos) - 1
		pattern := g.nextPattern()
		var comparisonLen int
		if prefixLen < bufPos+len(pattern) {
			comparisonLen = prefixLen - bufPos
		} else {
			comparisonLen = len(pattern)
		}
		if bufPos < prefixLen {
			if !bytes.Equal(prefix[bufPos:bufPos+comparisonLen], pattern[:comparisonLen]) {
				return false
			}
		}
	}

	if g.dataBit > 0 {
		g.dataP++
		g.dataBit = 0
	}
	postLoopPos := g.dataP
	g.dataP, g.dataBit = savePos, 0
	g.nextPos(true /* clean */) // Reset the state of huffman decoder
	// Second pass - we check spaces not covered by the patterns
	var lastUncovered int
	bufPos = 0
	for pos := g.nextPos(false /* clean */); pos != 0 && lastUncovered < prefixLen; pos = g.nextPos(false) {
		bufPos += int(pos) - 1
		if bufPos > lastUncovered {
			dif := uint64(bufPos - lastUncovered)
			var comparisonLen int
			if prefixLen < lastUncovered+int(dif) {
				comparisonLen = prefixLen - lastUncovered
			} else {
				comparisonLen = int(dif)
			}
			if !bytes.Equal(prefix[lastUncovered:lastUncovered+comparisonLen], g.data[postLoopPos:postLoopPos+uint64(comparisonLen)]) {
				return false
			}
			postLoopPos += dif
		}
		lastUncovered = bufPos + len(g.nextPattern())
	}
	if prefixLen > lastUncovered && int(wordLen) > lastUncovered {
		dif := wordLen - uint64(lastUncovered)
		var comparisonLen int
		if prefixLen < int(wordLen) {
			comparisonLen = prefixLen - lastUncovered
		} else {
			comparisonLen = int(dif)
		}
		if !bytes.Equal(prefix[lastUncovered:lastUncovered+comparisonLen], g.data[postLoopPos:postLoopPos+uint64(comparisonLen)]) {
			return false
		}
	}
	return true
}
