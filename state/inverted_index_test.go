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

package state

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	"github.com/ledgerwatch/erigon-lib/recsplit/eliasfano32"
	"github.com/ledgerwatch/log/v3"
	"github.com/stretchr/testify/require"
)

func testDbAndInvertedIndex(t *testing.T) (string, kv.RwDB, *InvertedIndex) {
	t.Helper()
	path := t.TempDir()
	logger := log.New()
	keysTable := "Keys"
	indexTable := "Index"
	db := mdbx.NewMDBX(logger).Path(path).WithTablessCfg(func(defaultBuckets kv.TableCfg) kv.TableCfg {
		return kv.TableCfg{
			keysTable:  kv.TableCfgItem{Flags: kv.DupSort},
			indexTable: kv.TableCfgItem{Flags: kv.DupSort},
		}
	}).MustOpen()
	ii, err := NewInvertedIndex(path, 16 /* aggregationStep */, "inv" /* filenameBase */, keysTable, indexTable)
	require.NoError(t, err)
	return path, db, ii
}

func TestInvIndexCollationBuild(t *testing.T) {
	_, db, ii := testDbAndInvertedIndex(t)
	defer db.Close()
	defer ii.Close()
	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()
	ii.SetTx(tx)

	ii.SetTxNum(2)
	err = ii.Add([]byte("key1"))
	require.NoError(t, err)

	ii.SetTxNum(3)
	err = ii.Add([]byte("key2"))
	require.NoError(t, err)

	ii.SetTxNum(6)
	err = ii.Add([]byte("key1"))
	require.NoError(t, err)
	err = ii.Add([]byte("key3"))
	require.NoError(t, err)

	err = tx.Commit()
	require.NoError(t, err)

	roTx, err := db.BeginRo(context.Background())
	require.NoError(t, err)
	defer roTx.Rollback()

	bs, err := ii.collate(0, 7, roTx)
	require.NoError(t, err)
	require.Equal(t, 3, len(bs))
	require.Equal(t, []uint64{3}, bs["key2"].ToArray())
	require.Equal(t, []uint64{2, 6}, bs["key1"].ToArray())
	require.Equal(t, []uint64{6}, bs["key3"].ToArray())

	sf, err := ii.buildFiles(0, bs)
	require.NoError(t, err)
	defer sf.Close()
	g := sf.decomp.MakeGetter()
	g.Reset(0)
	var words []string
	var intArrs [][]uint64
	for g.HasNext() {
		w, _ := g.Next(nil)
		words = append(words, string(w))
		w, _ = g.Next(w[:0])
		ef, _ := eliasfano32.ReadEliasFano(w)
		var ints []uint64
		it := ef.Iterator()
		for it.HasNext() {
			ints = append(ints, it.Next())
		}
		intArrs = append(intArrs, ints)
	}
	require.Equal(t, []string{"key1", "key2", "key3"}, words)
	require.Equal(t, [][]uint64{{2, 6}, {3}, {6}}, intArrs)
	r := recsplit.NewIndexReader(sf.index)
	for i := 0; i < len(words); i++ {
		offset := r.Lookup([]byte(words[i]))
		g.Reset(offset)
		w, _ := g.Next(nil)
		require.Equal(t, words[i], string(w))
	}
}

func TestInvIndexAfterPrune(t *testing.T) {
	_, db, ii := testDbAndInvertedIndex(t)
	defer db.Close()
	defer ii.Close()
	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	ii.SetTx(tx)

	ii.SetTxNum(2)
	err = ii.Add([]byte("key1"))
	require.NoError(t, err)

	ii.SetTxNum(3)
	err = ii.Add([]byte("key2"))
	require.NoError(t, err)

	ii.SetTxNum(6)
	err = ii.Add([]byte("key1"))
	require.NoError(t, err)
	err = ii.Add([]byte("key3"))
	require.NoError(t, err)

	err = tx.Commit()
	require.NoError(t, err)

	roTx, err := db.BeginRo(context.Background())
	require.NoError(t, err)
	defer roTx.Rollback()

	bs, err := ii.collate(0, 16, roTx)
	require.NoError(t, err)

	sf, err := ii.buildFiles(0, bs)
	require.NoError(t, err)
	defer sf.Close()

	tx, err = db.BeginRw(context.Background())
	require.NoError(t, err)
	ii.SetTx(tx)

	ii.integrateFiles(sf, 0, 16)

	err = ii.prune(0, 16)
	require.NoError(t, err)
	err = tx.Commit()
	require.NoError(t, err)
	tx, err = db.BeginRw(context.Background())
	require.NoError(t, err)
	ii.SetTx(tx)

	for _, table := range []string{ii.indexKeysTable, ii.indexTable} {
		var cur kv.Cursor
		cur, err = tx.Cursor(table)
		require.NoError(t, err)
		defer cur.Close()
		var k []byte
		k, _, err = cur.First()
		require.NoError(t, err)
		require.Nil(t, k, table)
	}
}

func filledInvIndex(t *testing.T) (string, kv.RwDB, *InvertedIndex, uint64) {
	t.Helper()
	path, db, ii := testDbAndInvertedIndex(t)
	tx, err := db.BeginRw(context.Background())
	require.NoError(t, err)
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	ii.SetTx(tx)
	txs := uint64(1000)
	// keys are encodings of numbers 1..31
	// each key changes value on every txNum which is multiple of the key
	for txNum := uint64(1); txNum <= txs; txNum++ {
		ii.SetTxNum(txNum)
		for keyNum := uint64(1); keyNum <= uint64(31); keyNum++ {
			if txNum%keyNum == 0 {
				var k [8]byte
				binary.BigEndian.PutUint64(k[:], keyNum)
				err = ii.Add(k[:])
				require.NoError(t, err)
			}
		}
		if txNum%10 == 0 {
			err = tx.Commit()
			require.NoError(t, err)
			tx, err = db.BeginRw(context.Background())
			require.NoError(t, err)
			ii.SetTx(tx)
		}
	}
	err = tx.Commit()
	require.NoError(t, err)
	return path, db, ii, txs
}

func checkRanges(t *testing.T, db kv.RwDB, ii *InvertedIndex, txs uint64) {
	t.Helper()
	ic := ii.MakeContext()
	// Check the iterator ranges first without roTx
	for keyNum := uint64(1); keyNum <= uint64(31); keyNum++ {
		var k [8]byte
		binary.BigEndian.PutUint64(k[:], keyNum)
		it := ic.IterateRange(k[:], 0, 976, nil)
		defer it.Close()
		for i := keyNum; i < 976; i += keyNum {
			label := fmt.Sprintf("keyNum=%d, txNum=%d", keyNum, i)
			require.True(t, it.HasNext(), label)
			n := it.Next()
			require.Equal(t, i, n, label)
		}
		require.False(t, it.HasNext())
	}
	// Now check ranges that require access to DB
	roTx, err := db.BeginRo(context.Background())
	require.NoError(t, err)
	defer roTx.Rollback()
	for keyNum := uint64(1); keyNum <= uint64(31); keyNum++ {
		var k [8]byte
		binary.BigEndian.PutUint64(k[:], keyNum)
		it := ic.IterateRange(k[:], 400, 1000, roTx)
		defer it.Close()
		for i := keyNum * ((400 + keyNum - 1) / keyNum); i < txs; i += keyNum {
			label := fmt.Sprintf("keyNum=%d, txNum=%d", keyNum, i)
			require.True(t, it.HasNext(), label)
			n := it.Next()
			require.Equal(t, i, n, label)
		}
		require.False(t, it.HasNext())
	}
}

func mergeInverted(t *testing.T, db kv.RwDB, ii *InvertedIndex, txs uint64) {
	t.Helper()
	// Leave the last 2 aggregation steps un-collated
	var tx kv.RwTx
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	// Leave the last 2 aggregation steps un-collated
	for step := uint64(0); step < txs/ii.aggregationStep-1; step++ {
		func() {
			roTx, err := db.BeginRo(context.Background())
			require.NoError(t, err)
			defer roTx.Rollback()
			bs, err := ii.collate(step*ii.aggregationStep, (step+1)*ii.aggregationStep, roTx)
			require.NoError(t, err)
			roTx.Rollback()
			sf, err := ii.buildFiles(step, bs)
			require.NoError(t, err)
			ii.integrateFiles(sf, step*ii.aggregationStep, (step+1)*ii.aggregationStep)
			tx, err = db.BeginRw(context.Background())
			require.NoError(t, err)
			ii.SetTx(tx)
			err = ii.prune(step*ii.aggregationStep, (step+1)*ii.aggregationStep)
			require.NoError(t, err)
			err = tx.Commit()
			require.NoError(t, err)
			tx = nil
			var found bool
			var startTxNum, endTxNum uint64
			maxEndTxNum := ii.endTxNumMinimax()
			maxSpan := uint64(16 * 16)
			for found, startTxNum, endTxNum = ii.findMergeRange(maxEndTxNum, maxSpan); found; found, startTxNum, endTxNum = ii.findMergeRange(maxEndTxNum, maxSpan) {
				outs, _ := ii.staticFilesInRange(startTxNum, endTxNum)
				in, err := ii.mergeFiles(outs, startTxNum, endTxNum, maxSpan)
				require.NoError(t, err)
				ii.integrateMergedFiles(outs, in)
				err = ii.deleteFiles(outs)
				require.NoError(t, err)
			}
		}()
	}
}

func TestInvIndexRanges(t *testing.T) {
	_, db, ii, txs := filledInvIndex(t)
	defer db.Close()
	defer ii.Close()
	var tx kv.RwTx
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()

	// Leave the last 2 aggregation steps un-collated
	for step := uint64(0); step < txs/ii.aggregationStep-1; step++ {
		func() {
			roTx, err := db.BeginRo(context.Background())
			require.NoError(t, err)
			bs, err := ii.collate(step*ii.aggregationStep, (step+1)*ii.aggregationStep, roTx)
			roTx.Rollback()
			require.NoError(t, err)
			sf, err := ii.buildFiles(step, bs)
			require.NoError(t, err)
			ii.integrateFiles(sf, step*ii.aggregationStep, (step+1)*ii.aggregationStep)
			tx, err = db.BeginRw(context.Background())
			require.NoError(t, err)
			ii.SetTx(tx)
			err = ii.prune(step*ii.aggregationStep, (step+1)*ii.aggregationStep)
			require.NoError(t, err)
			err = tx.Commit()
			require.NoError(t, err)
		}()
	}
	checkRanges(t, db, ii, txs)
}

func TestInvIndexMerge(t *testing.T) {
	_, db, ii, txs := filledInvIndex(t)
	defer db.Close()
	defer ii.Close()

	mergeInverted(t, db, ii, txs)
	checkRanges(t, db, ii, txs)
}

func TestInvIndexScanFiles(t *testing.T) {
	path, db, ii, txs := filledInvIndex(t)
	defer db.Close()
	defer func() {
		ii.Close()
	}()
	ii.Close()
	// Recreate InvertedIndex to scan the files
	var err error
	ii, err = NewInvertedIndex(path, ii.aggregationStep, ii.filenameBase, ii.indexKeysTable, ii.indexTable)
	require.NoError(t, err)

	mergeInverted(t, db, ii, txs)
	checkRanges(t, db, ii, txs)
}

func TestChangedKeysIterator(t *testing.T) {
	_, db, ii, txs := filledInvIndex(t)
	defer db.Close()
	defer func() {
		ii.Close()
	}()
	mergeInverted(t, db, ii, txs)
	roTx, err := db.BeginRo(context.Background())
	require.NoError(t, err)
	defer func() {
		roTx.Rollback()
	}()
	ic := ii.MakeContext()
	it := ic.IterateChangedKeys(0, 20, roTx)
	defer func() {
		it.Close()
	}()
	var keys []string
	for it.HasNext() {
		k := it.Next(nil)
		keys = append(keys, fmt.Sprintf("%x", k))
	}
	it.Close()
	require.Equal(t, []string{
		"0000000000000001",
		"0000000000000002",
		"0000000000000003",
		"0000000000000004",
		"0000000000000005",
		"0000000000000006",
		"0000000000000007",
		"0000000000000008",
		"0000000000000009",
		"000000000000000a",
		"000000000000000b",
		"000000000000000c",
		"000000000000000d",
		"000000000000000e",
		"000000000000000f",
		"0000000000000010",
		"0000000000000011",
		"0000000000000012"}, keys)
	it = ic.IterateChangedKeys(995, 1000, roTx)
	keys = keys[:0]
	for it.HasNext() {
		k := it.Next(nil)
		keys = append(keys, fmt.Sprintf("%x", k))
	}
	it.Close()
	require.Equal(t, []string{
		"0000000000000001",
		"0000000000000002",
		"0000000000000003",
		"0000000000000004",
		"0000000000000005",
		"0000000000000006",
		"0000000000000009",
		"000000000000000c",
	}, keys)
}
