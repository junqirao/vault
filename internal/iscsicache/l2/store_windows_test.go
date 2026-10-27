//go:build windows

package l2

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// These tests are the only ones that exercise the real unbuffered Windows store
// (the rest of the suite injects a MemoryStore), so they are what catches an
// alignment, sharing-mode or sharding mistake.

const (
	realBlock = 16 << 10
	realSect  = 4 << 10
	// Four slots, one index page and the super block.
	realSize = SuperBlockSize + indexPageSize + 4*(realBlock+stampSize)
)

func realStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cache.bin")
}

// storeSize reads the file size behind the Store interface, which only exposes
// the operations the L2 layer itself needs.
func storeSize(t *testing.T, s Store) int64 {
	t.Helper()
	ws, ok := s.(*winStore)
	if !ok {
		t.Fatalf("store is %T, want *winStore", s)
	}
	size, err := ws.fileSize()
	if err != nil {
		t.Fatalf("fileSize: %v", err)
	}
	return size
}

func TestStoreFileRoundTrip(t *testing.T) {
	path := realStorePath(t)
	s, err := openStore(path, realBlock)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	// A 4 KiB metadata write and a 16 KiB data write, sized and offset the way
	// the L2 issues them (stamp first, then the data area of the slot).
	stamp := bytes.Repeat([]byte{0x5A}, stampSize)
	data := bytes.Repeat([]byte{0xC3}, realBlock)
	if err := s.WriteAt(stamp, SuperBlockSize); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	if err := s.WriteAt(data, SuperBlockSize+int64(stampSize)); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := s.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// The file grows to what was written, not to the budget: an opened cache
	// must not reserve the space it may never use.
	if size, want := storeSize(t, s), int64(SuperBlockSize+stampSize+realBlock); size != want {
		t.Fatalf("fileSize = %d, want %d", size, want)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again, err := openStore(path, realBlock)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	gotStamp := make([]byte, stampSize)
	gotData := make([]byte, realBlock)
	if err := again.ReadAt(gotStamp, SuperBlockSize); err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if err := again.ReadAt(gotData, SuperBlockSize+int64(stampSize)); err != nil {
		t.Fatalf("read data: %v", err)
	}
	if !bytes.Equal(gotStamp, stamp) {
		t.Fatal("stamp did not survive a close")
	}
	if !bytes.Equal(gotData, data) {
		t.Fatal("data did not survive a close")
	}
	// Truncate is what the small-budget path uses; it has to hold the shards.
	if err := again.Truncate(realSize / 2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if size := storeSize(t, again); size != realSize/2 {
		t.Fatalf("fileSize after truncate = %d, want %d", size, realSize/2)
	}
}

// TestStoreConcurrentTransfers runs the shape that matters: many data transfers
// plus small metadata writes on interleaved offsets. Sharding has to keep them
// independent, and every region has to end up holding exactly its own pattern.
func TestStoreConcurrentTransfers(t *testing.T) {
	const (
		writers = 8
		rounds  = 64
		stride  = 2 * realBlock
		metaOps = writers * 16
	)
	// Data regions first, then a metadata region of its own: writers must never
	// overlap each other or the stamp writer, or a "mismatch" would be a legal
	// interleaving rather than a bug.
	metaBase := int64(SuperBlockSize) + writers*stride
	s, err := openStore(realStorePath(t), realBlock)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, writers+1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < metaOps; i++ {
			off := metaBase + int64(i)*int64(realBlock)
			if err := s.WriteAt(bytes.Repeat([]byte{byte(i)}, stampSize), off); err != nil {
				errCh <- fmt.Errorf("metadata write: %w", err)
				return
			}
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			base := int64(SuperBlockSize) + int64(w)*stride + int64(stampSize)
			buf := bytes.Repeat([]byte{byte(w + 1)}, realBlock)
			for r := 0; r < rounds; r++ {
				if err := s.WriteAt(buf, base); err != nil {
					errCh <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
				got := make([]byte, realBlock)
				if err := s.ReadAt(got, base); err != nil {
					errCh <- fmt.Errorf("reader %d: %w", w, err)
					return
				}
				if !bytes.Equal(got, buf) {
					errCh <- fmt.Errorf("writer %d read another writer's bytes", w)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	for w := 0; w < writers; w++ {
		base := int64(SuperBlockSize) + int64(w)*stride + int64(stampSize)
		got := make([]byte, realBlock)
		if err := s.ReadAt(got, base); err != nil {
			t.Fatalf("final read of writer %d: %v", w, err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{byte(w + 1)}, realBlock)) {
			t.Fatalf("writer %d lost its block to a neighbouring shard", w)
		}
	}
	// The metadata writes landed too, each in its own stamp sized region.
	for i := 0; i < metaOps; i++ {
		got := make([]byte, stampSize)
		if err := s.ReadAt(got, metaBase+int64(i)*int64(realBlock)); err != nil {
			t.Fatalf("metadata read %d: %v", i, err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, stampSize)) {
			t.Fatalf("metadata write %d was lost", i)
		}
	}
}

// TestFileOnRealStore drives the whole L2 layer over the real store, which is
// what validates that every offset the layer derives is legal unbuffered IO.
func TestFileOnRealStore(t *testing.T) {
	path := realStorePath(t)
	open := func() *File {
		t.Helper()
		f, err := Open(Config{
			Path:       path,
			SizeBytes:  realSize,
			BlockSize:  realBlock,
			SectorSize: realSect,
			Namespace:  "real-ns",
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return f
	}
	want := bytes.Repeat([]byte{0x7E}, realBlock)
	f := open()
	if err := f.WriteSectors(1, 0, 2, want[:2*realSect]); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A clean close is restored trusted and served straight from the file.
	live := open()
	got := make([]byte, 2*realSect)
	if ok, err := live.ReadSectors(1, 0, 2, got); err != nil || !ok {
		t.Fatalf("read back = %v, %v", ok, err)
	}
	if !bytes.Equal(got, want[:2*realSect]) {
		t.Fatal("a cleanly closed slot did not read back")
	}
	// Invalidating writes the slot stamp, and the close that follows writes the
	// index page: the drop has to survive both. The crash window itself is
	// covered by the MemoryStore tests, which can abandon a file in place.
	live.InvalidateSectors(1, 1, 1)
	if err := live.Close(); err != nil {
		t.Fatalf("close after invalidate: %v", err)
	}
	again := open()
	defer again.Close()
	if ok, _ := again.ReadSectors(1, 0, 2, make([]byte, 2*realSect)); ok {
		t.Fatal("the invalidated sector came back with the rest of the slot")
	}
	one := make([]byte, realSect)
	if ok, err := again.ReadSectors(1, 0, 1, one); err != nil || !ok {
		t.Fatalf("the surviving sector = %v, %v", ok, err)
	}
	if !bytes.Equal(one, want[:realSect]) {
		t.Fatal("the surviving sector of a partly invalidated slot is wrong")
	}
}

// TestOpenDoesNotReserveBudget pins that opening a cache no longer extends the
// file to its budget. A handful of libraries with large budgets used to take
// that much disk before a single block had been read.
func TestOpenDoesNotReserveBudget(t *testing.T) {
	const budget = 64 << 20
	path := filepath.Join(t.TempDir(), "l2.bin")
	f, err := Open(Config{
		Path:       path,
		SizeBytes:  budget,
		BlockSize:  64 << 10,
		SectorSize: realSect,
		Namespace:  "ns",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()

	stat := func(name string) int64 {
		t.Helper()
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		return info.Size()
	}

	if got := stat(path); got != SuperBlockSize {
		t.Fatalf("file after open = %d bytes, want just the superblock (%d)", got, SuperBlockSize)
	}
	// Sparse is what keeps a write near the end of the file from materialising
	// everything between it and the start.
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	attrs, err := windows.GetFileAttributes(ptr)
	if err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if attrs&windows.FILE_ATTRIBUTE_SPARSE_FILE == 0 {
		t.Fatal("the cache file is not sparse")
	}

	if err := f.WriteSectors(0, 0, 1, bytes.Repeat([]byte{0xA5}, realSect)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The file grows with the write, but stays far below the budget: one slot
	// is 64 KiB against a 64 MiB budget.
	after := stat(path)
	if after <= SuperBlockSize {
		t.Fatalf("file did not grow with the first write: %d", after)
	}
	if after >= budget {
		t.Fatalf("file grew to the budget: %d >= %d", after, budget)
	}

	// The index lives in its own file, so closing writes its page there instead
	// of stretching the data file to an offset near the end of the budget.
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed = true
	if got := stat(path); got != after {
		t.Fatalf("data file changed on close: %d -> %d", after, got)
	}
	idx := indexPath(path)
	if got, want := stat(idx), int64(indexPageSize); got != want {
		t.Fatalf("index file = %d bytes, want one page (%d)", got, want)
	}
}

// TestDiscardedSnapshotGivesSpaceBack pins that a file whose contents are
// discarded, as one written by an older layout is, does not keep the space it
// held. That is what reclaims a preallocated cache of tens of gigabytes on the
// first open under the new layout.
func TestDiscardedSnapshotGivesSpaceBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l2.bin")
	// Eight megabytes of allocated bytes with no valid superblock: the shape an
	// older, preallocated cache has from the new layout's point of view.
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x11}, 8<<20), 0o644); err != nil {
		t.Fatalf("write old cache: %v", err)
	}
	f, err := Open(Config{
		Path:       path,
		SizeBytes:  64 << 20,
		BlockSize:  64 << 10,
		SectorSize: realSect,
		Namespace:  "ns",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != SuperBlockSize {
		t.Fatalf("file after discarding = %d bytes, want %d", info.Size(), SuperBlockSize)
	}
}
