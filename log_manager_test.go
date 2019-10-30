/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/badger/y"
	humanize "github.com/dustin/go-humanize"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/trace"
)

func TestValueBasic(t *testing.T) {
	runBadgerTest(t, nil, func(t *testing.T, db *DB) {
		// Use value big enough that the value log writes them even if SyncWrites is false.
		const val1 = "sampleval012345678901234567890123"
		const val2 = "samplevalb012345678901234567890123"
		require.True(t, len(val1) >= db.opt.ValueThreshold)

		e1 := &Entry{
			Key:   []byte("samplekey"),
			Value: []byte(val1),
			meta:  bitValuePointer,
		}
		e2 := &Entry{
			Key:   []byte("samplekeyb"),
			Value: []byte(val2),
			meta:  bitValuePointer,
		}

		b := new(request)
		b.Entries = []*Entry{e1, e2}
		db.log.write([]*request{b})
		require.Len(t, b.Ptrs, 2)
		s := new(y.Slice)
		buf1, lf1, err1 := db.log.readValueBytes(b.Ptrs[0], s)
		buf2, lf2, err2 := db.log.readValueBytes(b.Ptrs[1], s)
		require.NoError(t, err1)
		require.NoError(t, err2)
		defer runCallback(db.log.getUnlockCallback(lf1))
		defer runCallback(db.log.getUnlockCallback(lf2))
		e1, err := lf1.decodeEntry(buf1, b.Ptrs[0].Offset)
		require.NoError(t, err)
		e2, err = lf1.decodeEntry(buf2, b.Ptrs[1].Offset)
		require.NoError(t, err)
		readEntries := []Entry{*e1, *e2}
		require.EqualValues(t, []Entry{
			{
				Key:    []byte("samplekey"),
				Value:  []byte(val1),
				meta:   bitValuePointer,
				offset: b.Ptrs[0].Offset,
			},
			{
				Key:    []byte("samplekeyb"),
				Value:  []byte(val2),
				meta:   bitValuePointer,
				offset: b.Ptrs[1].Offset,
			},
		}, readEntries)
	})
}

func TestValueGCManaged(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	N := 10000
	opt := getTestOptions(dir)
	opt.ValueLogMaxEntries = uint32(N / 10)
	opt.managedTxns = true
	db, err := Open(opt)
	require.NoError(t, err)
	defer db.Close()

	var ts uint64
	newTs := func() uint64 {
		ts++
		return ts
	}

	sz := 64 << 10
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])

		wg.Add(1)
		txn := db.NewTransactionAt(newTs(), true)
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v)))
		require.NoError(t, txn.CommitAt(newTs(), func(err error) {
			wg.Done()
			require.NoError(t, err)
		}))
	}

	for i := 0; i < N; i++ {
		wg.Add(1)
		txn := db.NewTransactionAt(newTs(), true)
		require.NoError(t, txn.Delete([]byte(fmt.Sprintf("key%d", i))))
		require.NoError(t, txn.CommitAt(newTs(), func(err error) {
			wg.Done()
			require.NoError(t, err)
		}))
	}
	wg.Wait()
	files, err := ioutil.ReadDir(dir)
	require.NoError(t, err)
	for _, fi := range files {
		t.Logf("File: %s. Size: %s\n", fi.Name(), humanize.Bytes(uint64(fi.Size())))
	}

	for i := 0; i < 100; i++ {
		// Try at max 100 times to GC even a single value log file.
		err := db.RunValueLogGC(0.0001)
		fmt.Println("I'm herer")
		fmt.Printf("%+v \n", err)
		if err == nil {
			return // Done
		}
	}
	require.Fail(t, "Unable to GC even a single value log file.")
}

func TestValueGC(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, _ := Open(opt)
	defer kv.Close()

	sz := 32 << 10
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v)))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 45; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	kv.log.filesLock.RLock()
	lf := kv.log.vlogFileMap[kv.log.sortedFids()[0]]
	kv.log.filesLock.RUnlock()

	//	lf.iterate(0, func(e Entry) bool {
	//		e.print("lf")
	//		return true
	//	})

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.log.rewrite(lf, tr)
	for i := 45; i < 100; i++ {
		key := []byte(fmt.Sprintf("key%d", i))

		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.True(t, len(val) == sz, "Size found: %d", len(val))
			return nil
		}))
	}
}

func TestValueGC2(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, _ := Open(opt)
	defer kv.Close()

	sz := 32 << 10
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v)))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 5; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	for i := 5; i < 10; i++ {
		v := []byte(fmt.Sprintf("value%d", i))
		txnSet(t, kv, []byte(fmt.Sprintf("key%d", i)), v, 0)
	}

	kv.log.filesLock.RLock()
	lf := kv.log.vlogFileMap[kv.log.sortedFids()[0]]
	kv.log.filesLock.RUnlock()

	//	lf.iterate(0, func(e Entry) bool {
	//		e.print("lf")
	//		return true
	//	})

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.log.rewrite(lf, tr)
	for i := 0; i < 5; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			_, err := txn.Get(key)
			require.Equal(t, ErrKeyNotFound, err)
			return nil
		}))
	}
	for i := 5; i < 10; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.Equal(t, string(val), fmt.Sprintf("value%d", i))
			return nil
		}))
	}
	for i := 10; i < 100; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.True(t, len(val) == sz, "Size found: %d", len(val))
			return nil
		}))
	}
}

func TestValueGC3(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20

	kv, err := Open(opt)
	require.NoError(t, err)
	defer kv.Close()

	// We want to test whether an iterator can continue through a value log GC.

	valueSize := 32 << 10

	var value3 []byte
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, valueSize) // 32K * 100 will take >=3'276'800 B.
		if i == 3 {
			value3 = v
		}
		rand.Read(v[:])
		// Keys key000, key001, key002, such that sorted order matches insertion order
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%03d", i)), v)))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	// Start an iterator to keys in the first value log file
	itOpt := IteratorOptions{
		PrefetchValues: false,
		PrefetchSize:   0,
		Reverse:        false,
	}

	txn = kv.NewTransaction(true)
	it := txn.NewIterator(itOpt)
	defer it.Close()
	// Walk a few keys
	it.Rewind()
	require.True(t, it.Valid())
	item := it.Item()
	require.Equal(t, []byte("key000"), item.Key())
	it.Next()
	require.True(t, it.Valid())
	item = it.Item()
	require.Equal(t, []byte("key001"), item.Key())
	it.Next()
	require.True(t, it.Valid())
	item = it.Item()
	require.Equal(t, []byte("key002"), item.Key())

	// Like other tests, we pull out a logFile to rewrite it directly

	kv.log.filesLock.RLock()
	logFile := kv.log.vlogFileMap[kv.log.sortedFids()[0]]
	kv.log.filesLock.RUnlock()

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.log.rewrite(logFile, tr)
	it.Next()
	require.True(t, it.Valid())
	item = it.Item()
	require.Equal(t, []byte("key003"), item.Key())

	v3, err := item.ValueCopy(nil)
	require.NoError(t, err)
	require.Equal(t, value3, v3)
}

func TestValueGC4(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20
	opt.Truncate = true

	kv, err := Open(opt)
	require.NoError(t, err)
	defer kv.Close()

	sz := 128 << 10 // 5 entries per value log file.
	txn := kv.NewTransaction(true)
	for i := 0; i < 24; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v)))
		if i%3 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 8; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	for i := 8; i < 16; i++ {
		v := []byte(fmt.Sprintf("value%d", i))
		txnSet(t, kv, []byte(fmt.Sprintf("key%d", i)), v, 0)
	}

	kv.log.filesLock.RLock()
	lf0 := kv.log.vlogFileMap[kv.log.sortedFids()[0]]
	lf1 := kv.log.vlogFileMap[kv.log.sortedFids()[1]]
	kv.log.filesLock.RUnlock()

	//	lf.iterate(0, func(e Entry) bool {
	//		e.print("lf")
	//		return true
	//	})

	tr := trace.New("Test", "Test")
	defer tr.Finish()
	kv.log.rewrite(lf0, tr)
	kv.log.rewrite(lf1, tr)

	err = kv.log.Close()
	require.NoError(t, err)

	manager, err := openLogManager(kv, valuePointer{Fid: 3, Offset: 20}, valuePointer{Fid: 3, Offset: 20}, kv.replayFunction())
	require.NoError(t, err)
	kv.log = manager

	for i := 0; i < 8; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			_, err := txn.Get(key)
			require.Equal(t, ErrKeyNotFound, err)
			return nil
		}))
	}
	for i := 8; i < 16; i++ {
		key := []byte(fmt.Sprintf("key%d", i))
		require.NoError(t, kv.View(func(txn *Txn) error {
			item, err := txn.Get(key)
			require.NoError(t, err)
			val := getItemValue(t, item)
			require.NotNil(t, val)
			require.Equal(t, string(val), fmt.Sprintf("value%d", i))
			return nil
		}))
	}
}

func TestPersistLFDiscardStats(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)
	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20
	opt.Truncate = true
	// avoid compaction on close, so that discard map remains same
	opt.CompactL0OnClose = false

	db, err := Open(opt)
	require.NoError(t, err)

	sz := 128 << 10 // 5 entries per value log file.
	v := make([]byte, sz)
	rand.Read(v[:rand.Intn(sz)])
	txn := db.NewTransaction(true)
	for i := 0; i < 500; i++ {
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v)))
		if i%3 == 0 {
			require.NoError(t, txn.Commit())
			txn = db.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit(), "error while committing txn")

	for i := 0; i < 500; i++ {
		// use Entry.WithDiscard() to delete entries, because this causes data to be flushed on
		// disk, creating SSTs. Simple Delete was having data in Memtables only.
		err = db.Update(func(txn *Txn) error {
			return txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v).WithDiscard())
		})
		require.NoError(t, err)
	}

	time.Sleep(1 * time.Second) // wait for compaction to complete

	persistedMap := make(map[uint32]int64)
	db.log.lfDiscardStats.Lock()
	require.True(t, len(db.log.lfDiscardStats.m) > 0, "some discardStats should be generated")
	for k, v := range db.log.lfDiscardStats.m {
		persistedMap[k] = v
	}
	db.log.lfDiscardStats.updatesSinceFlush = discardStatsFlushThreshold + 1
	db.log.lfDiscardStats.Unlock()

	// db.vlog.lfDiscardStats.updatesSinceFlush is already > discardStatsFlushThreshold,
	// send empty map to flushChan, so that latest discardStats map can be persisted.
	db.log.lfDiscardStats.flushChan <- map[uint32]int64{}
	time.Sleep(1 * time.Second) // Wait for map to be persisted.
	err = db.Close()
	require.NoError(t, err)

	db, err = Open(opt)
	require.NoError(t, err)
	defer db.Close()
	time.Sleep(1 * time.Second) // Wait for discardStats to be populated by populateDiscardStats().
	db.log.lfDiscardStats.RLock()
	require.True(t, reflect.DeepEqual(persistedMap, db.log.lfDiscardStats.m),
		"Discard maps are not equal")
	db.log.lfDiscardStats.RUnlock()
}

func TestChecksums(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	// Set up SST with K1=V1
	opts := getTestOptions(dir)
	opts.Truncate = true
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	require.NoError(t, kv.Close())

	var (
		k0 = []byte("k0")
		k1 = []byte("k1")
		k2 = []byte("k2")
		k3 = []byte("k3")
		v0 = []byte("value0-012345678901234567890123012345678901234567890123")
		v1 = []byte("value1-012345678901234567890123012345678901234567890123")
		v2 = []byte("value2-012345678901234567890123012345678901234567890123")
		v3 = []byte("value3-012345678901234567890123012345678901234567890123")
	)
	// Make sure the value log would actually store the item
	require.True(t, len(v0) >= kv.opt.ValueThreshold)

	// Use a vlog with K0=V0 and a (corrupted) second transaction(k1,k2)
	buf, buf2 := createLog(t, []*Entry{
		{Key: k0, Value: v0},
		{Key: k1, Value: v1},
		{Key: k2, Value: v2},
	})
	buf[len(buf)-1]++ // Corrupt last byte
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 1), buf, 0777))
	require.NoError(t, ioutil.WriteFile(walFilePath(dir, 1), buf2, 0777))
	// K1 should exist, but K2 shouldn't.
	kv, err = Open(opts)
	require.NoError(t, err)

	require.NoError(t, kv.View(func(txn *Txn) error {
		item, err := txn.Get(k0)
		require.NoError(t, err)
		require.Equal(t, getItemValue(t, item), v0)

		_, err = txn.Get(k1)
		require.Equal(t, ErrKeyNotFound, err)

		_, err = txn.Get(k2)
		require.Equal(t, ErrKeyNotFound, err)
		return nil
	}))

	// Write K3 at the end of the vlog.
	txnSet(t, kv, k3, v3, 0)
	require.NoError(t, kv.Close())

	// The vlog should contain K0 and K3 (K1 and k2 was lost when Badger started up
	// last due to checksum failure).
	kv, err = Open(opts)
	require.NoError(t, err)

	{
		txn := kv.NewTransaction(false)

		iter := txn.NewIterator(DefaultIteratorOptions)
		iter.Seek(k0)
		require.True(t, iter.Valid())
		it := iter.Item()
		require.Equal(t, it.Key(), k0)
		require.Equal(t, getItemValue(t, it), v0)
		iter.Next()
		require.True(t, iter.Valid())
		it = iter.Item()
		require.Equal(t, it.Key(), k3)
		require.Equal(t, getItemValue(t, it), v3)

		iter.Close()
		txn.Discard()
	}

	require.NoError(t, kv.Close())
}

func createLog(t *testing.T, entries []*Entry) ([]byte, []byte) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	opts := getTestOptions(dir)
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	txnSet(t, kv, entries[0].Key, entries[0].Value, entries[0].meta)
	entries = entries[1:]
	txn := kv.NewTransaction(true)
	for _, entry := range entries {
		require.NoError(t, txn.SetEntry(NewEntry(entry.Key, entry.Value).WithMeta(entry.meta)))
	}
	require.NoError(t, txn.Commit())
	require.NoError(t, kv.Close())

	filename := vlogFilePath(dir, 1)
	buf, err := ioutil.ReadFile(filename)
	require.NoError(t, err)

	filename = walFilePath(dir, 1)
	buf2, err := ioutil.ReadFile(filename)
	require.NoError(t, err)
	return buf, buf2
}

func TestPartialAppendToValueLog(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	// Create skeleton files.
	opts := getTestOptions(dir)
	opts.Truncate = true
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	require.NoError(t, kv.Close())

	var (
		k0 = []byte("k0")
		k1 = []byte("k1")
		k2 = []byte("k2")
		k3 = []byte("k3")
		v0 = []byte("value0-01234567890123456789012012345678901234567890123")
		v1 = []byte("value1-01234567890123456789012012345678901234567890123")
		v2 = []byte("value2-01234567890123456789012012345678901234567890123")
		v3 = []byte("value3-01234567890123456789012012345678901234567890123")
	)
	// Values need to be long enough to actually get written to value log.
	require.True(t, len(v3) >= kv.opt.ValueThreshold)

	// Create truncated vlog to simulate a partial append.
	// k0 - single transaction, k1 and k2 in another transaction
	buf, buf2 := createLog(t, []*Entry{
		{Key: k0, Value: v0},
		{Key: k1, Value: v1},
		{Key: k2, Value: v2},
	})
	buf = buf[:len(buf)-6]
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 1), buf, 0777))
	require.NoError(t, ioutil.WriteFile(walFilePath(dir, 1), buf2, 0777))
	// Badger should now start up
	kv, err = Open(opts)
	require.NoError(t, err)

	require.NoError(t, kv.View(func(txn *Txn) error {
		item, err := txn.Get(k0)
		require.NoError(t, err)
		require.Equal(t, v0, getItemValue(t, item))

		_, err = txn.Get(k1)
		require.Equal(t, ErrKeyNotFound, err)
		_, err = txn.Get(k2)
		require.Equal(t, ErrKeyNotFound, err)
		return nil
	}))

	// When K3 is set, it should be persisted after a restart.
	txnSet(t, kv, k3, v3, 0)
	require.NoError(t, kv.Close())
	kv, err = Open(opts)
	require.NoError(t, err)
	checkKeys(t, kv, [][]byte{k3})
	// Replay value log from beginning, badger head is past k2.
	require.NoError(t, kv.log.Close())

	log, err := openLogManager(kv, valuePointer{Fid: 1}, valuePointer{Fid: 1}, kv.replayFunction())
	require.NoError(t, err)
	kv.log = log
	require.NoError(t, kv.Close())
}

func checkKeys(t *testing.T, kv *DB, keys [][]byte) {
	i := 0
	txn := kv.NewTransaction(false)
	iter := txn.NewIterator(IteratorOptions{})
	for iter.Seek(keys[0]); iter.Valid(); iter.Next() {
		require.Equal(t, iter.Item().Key(), keys[i])
		i++
	}
	require.Equal(t, i, len(keys))
}

func TestReadOnlyOpenWithPartialAppendToValueLog(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	// Create skeleton files.
	opts := getTestOptions(dir)
	opts.ValueLogFileSize = 100 * 1024 * 1024 // 100Mb
	kv, err := Open(opts)
	require.NoError(t, err)
	require.NoError(t, kv.Close())

	var (
		k0 = []byte("k0")
		k1 = []byte("k1")
		k2 = []byte("k2")
		v0 = []byte("value0-012345678901234567890123")
		v1 = []byte("value1-012345678901234567890123")
		v2 = []byte("value2-012345678901234567890123")
	)

	// Create truncated vlog to simulate a partial append.
	// k0 - single transaction, k1 and k2 in another transaction
	buf, buf2 := createLog(t, []*Entry{
		{Key: k0, Value: v0},
		{Key: k1, Value: v1},
		{Key: k2, Value: v2},
	})
	buf = buf[:len(buf)-6]
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 1), buf, 0777))
	require.NoError(t, ioutil.WriteFile(walFilePath(dir, 1), buf2, 0777))
	opts.ReadOnly = true
	// Badger should fail a read-only open with values to replay
	_, err = Open(opts)
	require.Error(t, err)
	fmt.Printf("%+v \n", err)
	require.Regexp(t, "Database was not properly closed, cannot open read-only|Read-only mode is not supported on Windows", err.Error())
}

func TestValueLogTrigger(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	opt := getTestOptions(dir)
	opt.ValueLogFileSize = 1 << 20
	kv, err := Open(opt)
	require.NoError(t, err)

	// Write a lot of data, so it creates some work for valug log GC.
	sz := 32 << 10
	txn := kv.NewTransaction(true)
	for i := 0; i < 100; i++ {
		v := make([]byte, sz)
		rand.Read(v[:rand.Intn(sz)])
		require.NoError(t, txn.SetEntry(NewEntry([]byte(fmt.Sprintf("key%d", i)), v)))
		if i%20 == 0 {
			require.NoError(t, txn.Commit())
			txn = kv.NewTransaction(true)
		}
	}
	require.NoError(t, txn.Commit())

	for i := 0; i < 45; i++ {
		txnDelete(t, kv, []byte(fmt.Sprintf("key%d", i)))
	}

	require.NoError(t, kv.RunValueLogGC(0.5))

	require.NoError(t, kv.Close())

	err = kv.RunValueLogGC(0.5)
	require.Equal(t, ErrRejected, err, "Error should be returned after closing DB.")
}

type testHelper struct {
	db  *DB
	t   *testing.T
	val []byte
}

func (th *testHelper) key(i int) []byte {
	return []byte(fmt.Sprintf("%010d", i))
}
func (th *testHelper) value() []byte {
	if len(th.val) > 0 {
		return th.val
	}
	th.val = make([]byte, 100)
	y.Check2(rand.Read(th.val))
	return th.val
}

// writeRange [from, to].
func (th *testHelper) writeRange(from, to int) {
	for i := from; i <= to; i++ {
		err := th.db.Update(func(txn *Txn) error {
			return txn.SetEntry(NewEntry(th.key(i), th.value()))
		})
		require.NoError(th.t, err)
	}
}

func (th *testHelper) readRange(from, to int) {
	for i := from; i <= to; i++ {
		err := th.db.View(func(txn *Txn) error {
			item, err := txn.Get(th.key(i))
			if err != nil {
				return err
			}
			return item.Value(func(val []byte) error {
				require.Equal(th.t, val, th.value(), "key=%q", th.key(i))
				return nil

			})
		})
		require.NoError(th.t, err, "key=%q", th.key(i))
	}
}

// Test Bug #578, which showed that if a value is moved during value log GC, an
// older version can end up at a higher level in the LSM tree than a newer
// version, causing the data to not be returned.
func TestBug578(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	y.Check(err)
	defer removeDir(dir)

	db, err := Open(DefaultOptions(dir).
		WithValueLogMaxEntries(64).
		WithMaxTableSize(1 << 13))
	require.NoError(t, err)

	h := testHelper{db: db, t: t}

	// Let's run this whole thing a few times.
	for j := 0; j < 10; j++ {
		t.Logf("Cycle: %d\n", j)
		h.writeRange(0, 32)
		h.writeRange(0, 10)
		h.writeRange(50, 72)
		h.writeRange(40, 72)
		h.writeRange(40, 72)

		// Run value log GC a few times.
		for i := 0; i < 5; i++ {
			db.RunValueLogGC(0.5)
		}
		h.readRange(0, 10)
	}
	require.NoError(t, db.Close())
}

func BenchmarkReadWrite(b *testing.B) {
	rwRatio := []float32{
		0.1, 0.2, 0.5, 1.0,
	}
	valueSize := []int{
		64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384,
	}

	for _, vsz := range valueSize {
		for _, rw := range rwRatio {
			b.Run(fmt.Sprintf("%3.1f,%04d", rw, vsz), func(b *testing.B) {
				dir, err := ioutil.TempDir("", "vlog-benchmark")
				y.Check(err)
				defer removeDir(dir)

				db, err := Open(getTestOptions(dir))
				y.Check(err)

				vl := db.log
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					e := new(Entry)
					e.Key = make([]byte, 16)
					e.Value = make([]byte, vsz)
					bl := new(request)
					bl.Entries = []*Entry{e}

					var ptrs []valuePointer

					vl.write([]*request{bl})
					ptrs = append(ptrs, bl.Ptrs...)

					f := rand.Float32()
					if f < rw {
						bl.Ptrs = []valuePointer{}
						vl.write([]*request{bl})

					} else {
						ln := len(ptrs)
						if ln == 0 {
							b.Fatalf("Zero length of ptrs")
						}
						idx := rand.Intn(ln)
						s := new(y.Slice)
						buf, lf, err := vl.readValueBytes(ptrs[idx], s)
						if err != nil {
							b.Fatalf("Benchmark Read: %v", err)
						}

						e, err := lf.decodeEntry(buf, ptrs[idx].Offset)
						require.NoError(b, err)
						if len(e.Key) != 16 {
							b.Fatalf("Key is invalid")
						}
						if len(e.Value) != vsz {
							b.Fatalf("Value is invalid")
						}
						runCallback(db.log.getUnlockCallback(lf))
					}
				}
			})
		}
	}
}

// Regression test for https://github.com/dgraph-io/badger/issues/817
func TestValueLogTruncate(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	db, err := Open(DefaultOptions(dir).WithTruncate(true))
	require.NoError(t, err)
	// Insert 1 entry so that we have valid data in first vlog file
	require.NoError(t, db.Update(func(txn *Txn) error {
		return txn.Set([]byte("foo"), nil)
	}))

	fileCountBeforeCorruption := len(db.log.vlogFileMap)

	require.NoError(t, db.Close())

	// Create log files corrupted data. These will be truncated when DB starts next time
	require.NoError(t, ioutil.WriteFile(vlogFilePath(dir, 2), []byte("foo"), 0664))
	require.NoError(t, ioutil.WriteFile(walFilePath(dir, 2), []byte("foo"), 0664))

	db, err = Open(DefaultOptions(dir).WithTruncate(true))
	require.NoError(t, err)

	// Ensure filesize of fid=2 is zero
	zeroFile, ok := db.log.vlogFileMap[2]
	require.True(t, ok)
	fileStat, err := zeroFile.fd.Stat()
	require.NoError(t, err)

	// The size of last vlog file in windows is equal to 2*opt.ValueLogFileSize. This is because
	// we mmap the last value log file and windows doesn't allow us to mmap a file more than
	// it's acutal size. So we increase the file size and then mmap it. See mmap_windows.go file.
	if runtime.GOOS == "windows" {
		require.Equal(t, 2*db.opt.ValueLogFileSize, fileStat.Size())
	} else {
		require.Equal(t, int64(vlogHeaderSize), fileStat.Size())
	}
	fileCountAfterCorruption := len(db.log.vlogFileMap)
	// +1 because the file with id=2 will be completely truncated. It won't be deleted.
	// There would be two files. fid=0 with valid data, fid=2 with zero data (truncated).
	require.Equal(t, fileCountBeforeCorruption+1, fileCountAfterCorruption)
	// Max file ID would point to the last vlog file, which is fid=2 in this case
	require.Equal(t, 2, int(db.log.maxVlogID))
	require.Equal(t, 2, int(db.log.maxWalID))
	require.NoError(t, db.Close())
}

// Regression test for https://github.com/dgraph-io/dgraph/issues/3669
func TestTruncatedDiscardStat(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	ops := getTestOptions(dir)
	db, err := Open(ops)
	require.NoError(t, err)

	stat := make(map[uint32]int64, 20)
	for i := uint32(0); i < uint32(20); i++ {
		stat[i] = 0
	}
	db.log.lfDiscardStats.m = stat
	encodedDS, _ := json.Marshal(db.log.lfDiscardStats.m)
	entries := []*Entry{{
		Key: y.KeyWithTs(lfDiscardStatsKey, 1),
		// Insert truncated discard stats. This is important.
		Value: encodedDS[:10],
	}}
	// set finish mark for wal
	entries = append(entries, &Entry{
		Key:      y.KeyWithTs(txnKey, math.MaxUint64),
		Value:    []byte(strconv.FormatUint(math.MaxUint64, 10)),
		meta:     bitFinTxn,
		forceWal: true,
	})
	// set finish mark for vlog
	entries = append(entries, &Entry{
		Key:   y.KeyWithTs(txnKeyVlog, math.MaxUint64),
		Value: []byte(strconv.FormatUint(math.MaxUint64, 10)),
		meta:  bitFinTxn,
	})
	// Push discard stats entry to the write channel.
	req, err := db.sendToWriteCh(entries)
	require.NoError(t, err)
	req.Wait()

	// Unset discard stats. We've already pushed the stats. If we don't unset it then it will be
	// pushed again on DB close.
	db.log.lfDiscardStats.m = nil

	require.NoError(t, db.Close())

	db, err = Open(ops)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// Regression test for https://github.com/dgraph-io/badger/issues/926
func TestDiscardStatsMove(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	ops := getTestOptions(dir)
	ops.ValueLogMaxEntries = 1
	db, err := Open(ops)
	require.NoError(t, err)

	stat := make(map[uint32]int64, ops.ValueThreshold+10)
	for i := uint32(0); i < uint32(ops.ValueThreshold+10); i++ {
		stat[i] = 0
	}

	db.log.lfDiscardStats.Lock()
	db.log.lfDiscardStats.m = stat
	encodedDS, _ := json.Marshal(db.log.lfDiscardStats.m)
	db.log.lfDiscardStats.Unlock()
	entries := []*Entry{{
		Key: y.KeyWithTs(lfDiscardStatsKey, 1),
		// The discard stat value is more than value threshold.
		Value: encodedDS,
	}}
	// set finish mark for wal
	entries = append(entries, &Entry{
		Key:      y.KeyWithTs(txnKey, math.MaxUint64),
		Value:    []byte(strconv.FormatUint(math.MaxUint64, 10)),
		meta:     bitFinTxn,
		forceWal: true,
	})
	// set finish mark for vlog
	entries = append(entries, &Entry{
		Key:   y.KeyWithTs(txnKeyVlog, math.MaxUint64),
		Value: []byte(strconv.FormatUint(math.MaxUint64, 10)),
		meta:  bitFinTxn,
	})
	// Push discard stats entry to the write channel.
	req, err := db.sendToWriteCh(entries)
	require.NoError(t, err)
	req.Wait()

	// Unset discard stats. We've already pushed the stats. If we don't unset it then it will be
	// pushed again on DB close. Also, the first insertion was in vlog file 1, this insertion would
	// be in value log file 3.
	db.log.lfDiscardStats.Lock()
	db.log.lfDiscardStats.m = nil
	db.log.lfDiscardStats.Unlock()

	// Push more entries so that we get more than 1 value log files.
	require.NoError(t, db.Update(func(txn *Txn) error {
		e := NewEntry([]byte("f"), []byte("1"))
		return txn.SetEntry(e)
	}))
	require.NoError(t, db.Update(func(txn *Txn) error {
		e := NewEntry([]byte("ff"), []byte("1"))
		return txn.SetEntry(e)
	}))

	tr := trace.New("Badger.ValueLog", "GC")
	// Use first value log file for GC. This value log file contains the discard stats.
	lf := db.log.vlogFileMap[1]
	require.NoError(t, db.log.rewrite(lf, tr))
	require.NoError(t, db.Close())

	db, err = Open(ops)
	// discardStats will be populate using vlog.populateDiscardStats(), which pushes discard stats
	// to vlog.lfDiscardStats.flushChan. Hence wait for some time, for discard stats to be updated.
	time.Sleep(1 * time.Second)
	require.NoError(t, err)
	db.log.lfDiscardStats.RLock()
	require.Equal(t, stat, db.log.lfDiscardStats.m)
	db.log.lfDiscardStats.RUnlock()
	require.NoError(t, db.Close())
}

// This test ensures, flushDiscardStats() doesn't crash.
func TestBlockedDiscardStats(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer os.Remove(dir)
	db, err := Open(getTestOptions(dir))
	require.NoError(t, err)
	// Set discard stats.
	db.log.lfDiscardStats.m = map[uint32]int64{0: 0}
	db.blockWrite()
	// Push discard stats more than the capacity of flushChan. This ensures at least one flush
	// operation completes successfully after the writes were blocked.
	for i := 0; i < cap(db.log.lfDiscardStats.flushChan)+2; i++ {
		db.log.lfDiscardStats.flushChan <- db.log.lfDiscardStats.m
	}
	db.unblockWrite()
	require.NoError(t, db.Close())
}

// Regression test for https://github.com/dgraph-io/badger/issues/970
func TestBlockedDiscardStatsOnClose(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer os.Remove(dir)

	db, err := Open(getTestOptions(dir))
	require.NoError(t, err)
	db.log.lfDiscardStats.m = map[uint32]int64{0: 0}
	// This is important. Set updateSinceFlush to discardStatsFlushThreshold so
	// that the next update call flushes the discard stats.
	db.log.lfDiscardStats.updatesSinceFlush = discardStatsFlushThreshold + 1
	require.NoError(t, db.Close())
}

// Regression test for https://github.com/dgraph-io/badger/issues/1049
func TestValueEntryCorruption(t *testing.T) {
	dir, err := ioutil.TempDir("", "badger-test")
	require.NoError(t, err)
	defer removeDir(dir)

	opt := getTestOptions(dir)
	opt.VerifyValueChecksum = true
	db, err := Open(opt)
	require.NoError(t, err)

	k := []byte("KEY")
	v := []byte(fmt.Sprintf("val%100d", 10))
	require.Greater(t, len(v), db.opt.ValueThreshold)
	txnSet(t, db, k, v, 0)

	path := vlogFilePath(db.opt.ValueDir, 1)
	require.NoError(t, db.Close())

	file, err := os.OpenFile(path, os.O_RDWR, 0644)
	require.NoError(t, err)
	offset := 50
	orig := make([]byte, 1)
	_, err = file.ReadAt(orig, int64(offset))
	require.NoError(t, err)
	// Corrupt a single bit.
	_, err = file.WriteAt([]byte{7}, int64(offset))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	db, err = Open(opt)
	require.NoError(t, err)

	txn := db.NewTransaction(false)
	entry, err := txn.Get(k)
	require.NoError(t, err)

	x, err := entry.ValueCopy(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksum mismatch")
	require.Nil(t, x)

	require.NoError(t, db.Close())
}
