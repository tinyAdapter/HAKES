/*
 * Copyright 2024 The HAKES Authors
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

package table

import (
	"fmt"
	"log"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	fbs "github.com/google/flatbuffers/go"
	"github.com/pkg/errors"

	"github.com/dgraph-io/badger/v3/options"
	"github.com/dgraph-io/badger/v3/pb"
	"github.com/dgraph-io/badger/v3/y"
	"github.com/dgraph-io/ristretto/z"

	io "hakes-store/hakes-store/io"
	fb "hakes-store/hakes-store/table/fb"
)

const (
	KB = 1024
	MB = KB * 1024

	// When a block is encrypted, it's length increases. We add 256 bytes of padding to
	// handle cases when block size increases. This is an approximate number.
	padding = 256
)

type header struct {
	overlap uint16 // Overlap with base key.
	diff    uint16 // Length of the diff.
}

const headerSize = uint16(unsafe.Sizeof(header{}))

// Encode encodes the header.
func (h header) Encode() []byte {
	var b [4]byte
	*(*header)(unsafe.Pointer(&b[0])) = h
	return b[:]
}

// Decode decodes the header.
func (h *header) Decode(buf []byte) {
	copy(((*[headerSize]byte)(unsafe.Pointer(h))[:]), buf[:headerSize])
}

// bblock represents a block that is being compressed/encrypted in the background.
type bblock struct {
	id           string
	data         []byte
	baseKey      []byte // Base key for the current block.
	maxKey       []byte
	entryOffsets []uint32 // Offsets of entries present in current block.
	end          int      // Points to the end offset of the block.
	shadow       bool     // simply adding an indicator to avoids differentiating empty and shadow bblock from data or entryOffsets.
}

func (b *bblock) String() string {
	return fmt.Sprintf("block id: %v, minkey %v(%x), maxkey %v(%x), entries: %d, shadow %v", b.id, string(y.ParseKey(b.baseKey)), b.baseKey, string(y.ParseKey(b.maxKey)), b.maxKey, len(b.entryOffsets), b.shadow)
}

type keyDiffCtx struct {
	overlap uint16
	diff    []byte
}

// Builder is used in building a table.
type Builder struct {
	// Typically tens or hundreds of meg. This is for one single file.
	alloc            *z.Allocator
	curBlock         *bblock
	compressedSize   uint32
	uncompressedSize uint32

	// filter
	keyHashes   []uint32 // Used for building the bloomfilter.
	keyHashList [][]uint32
	filterList  []*y.Filter
	filterWg    sync.WaitGroup

	lenOffsets    uint32
	opts          *Options
	maxVersion    uint64
	onDiskSize    uint32
	staleDataSize int

	// Used to concurrently compress/encrypt blocks.
	wg        sync.WaitGroup
	blockChan chan *bblock
	blockList []*bblock

	// last key used to finalize maxkey of a block
	lastKey keyDiffCtx
}

func (b *Builder) allocate(need int) []byte {
	bb := b.curBlock
	if len(bb.data[bb.end:]) < need {
		// We need to reallocate. 1GB is the max size that the allocator can allocate.
		// While reallocating, if doubling exceeds that limit, then put the upper bound on it.
		sz := 2 * len(bb.data)
		if sz > (1 << 30) {
			sz = 1 << 30
		}
		if bb.end+need > sz {
			sz = bb.end + need
		}
		tmp := b.alloc.Allocate(sz)
		copy(tmp, bb.data)
		bb.data = tmp
	}
	bb.end += need
	return bb.data[bb.end-need : bb.end]
}

// append appends to curBlock.data
func (b *Builder) append(data []byte) {
	dst := b.allocate(len(data))
	y.AssertTrue(len(data) == copy(dst, data))
}

const maxAllocatorInitialSz = 256 << 20

// NewTableBuilder makes a new TableBuilder.
func NewTableBuilder(opts Options, uid string) *Builder {
	sz := 2 * int(opts.TableSize)
	if sz > maxAllocatorInitialSz {
		sz = maxAllocatorInitialSz
	}
	b := &Builder{
		alloc: opts.AllocPool.Get(sz, "TableBuilder"),
		opts:  &opts,
	}
	b.alloc.Tag = "Builder"
	b.curBlock = &bblock{
		id:   "",
		data: b.alloc.Allocate(opts.BlockSize + padding),
	}
	b.opts.tableCapacity = uint64(float64(b.opts.TableSize) * 0.95)
	b.keyHashList = append(b.keyHashList, []uint32{})
	b.keyHashes = b.keyHashList[0]

	if b.opts.Compression == options.None {
		return b
	}

	count := 2 * runtime.NumCPU()
	b.blockChan = make(chan *bblock, count*2)

	b.wg.Add(count)
	for i := 0; i < count; i++ {
		go b.handleBlock()
	}

	return b
}

func (b *Builder) handleBlock() {
	defer b.wg.Done()

	doCompress := b.opts.Compression != options.None
	for item := range b.blockChan {
		// Extract the block.
		blockBuf := item.data[:item.end]
		// Compress the block.
		if doCompress {
			out, err := b.compressData(blockBuf)
			y.Check(err)
			blockBuf = out
		}

		// BlockBuf should always less than or equal to allocated space. If the blockBuf is greater
		// than allocated space that means the data from this block cannot be stored in its
		// existing location.
		allocatedSpace := (item.end) + padding + 1
		y.AssertTrue(len(blockBuf) <= allocatedSpace)

		// blockBuf was allocated on allocator. So, we don't need to copy it over.
		item.data = blockBuf
		item.end = len(blockBuf)
		atomic.AddUint32(&b.compressedSize, uint32(len(blockBuf)))
	}
}

// Close closes the TableBuilder.
func (b *Builder) Close() {
	b.opts.AllocPool.Return(b.alloc)
}

// Empty returns whether it's empty.
func (b *Builder) Empty() bool {
	return len(b.keyHashes) == 0 && len(b.blockList) == 0
}

// keyDiff returns a suffix of newKey that is different from b.baseKey.
func (b *Builder) keyDiff(newKey []byte) []byte {
	var i int
	for i = 0; i < len(newKey) && i < len(b.curBlock.baseKey); i++ {
		if newKey[i] != b.curBlock.baseKey[i] {
			break
		}
	}
	return newKey[i:]
}

func (b *Builder) addHelper(key []byte, v y.ValueStruct, vpLen uint32) {
	b.keyHashes = append(b.keyHashes, y.Hash(y.ParseKey(key)))

	if version := y.ParseTs(key); version > b.maxVersion {
		b.maxVersion = version
	}

	// diffKey stores the difference of key with baseKey.
	var diffKey []byte
	if len(b.curBlock.baseKey) == 0 {
		// Make a copy. Builder should not keep references. Otherwise, caller has to be very careful
		// and will have to make copies of keys every time they add to builder, which is even worse.
		b.curBlock.baseKey = append(b.curBlock.baseKey[:0], key...)
		diffKey = key
	} else {
		diffKey = b.keyDiff(key)
	}

	y.AssertTrue(len(key)-len(diffKey) <= math.MaxUint16)
	y.AssertTrue(len(diffKey) <= math.MaxUint16)

	h := header{
		overlap: uint16(len(key) - len(diffKey)),
		diff:    uint16(len(diffKey)),
	}

	// store current entry's offset
	b.curBlock.entryOffsets = append(b.curBlock.entryOffsets, uint32(b.curBlock.end))

	// Layout: header, diffKey, value.
	b.append(h.Encode())
	b.append(diffKey)
	// update the max key
	b.lastKey = keyDiffCtx{overlap: h.overlap, diff: b.curBlock.data[b.curBlock.end-len(diffKey) : b.curBlock.end]}

	dst := b.allocate(int(v.EncodedSize()))
	v.Encode(dst)

	// Add the vpLen to the onDisk size. We'll add the size of the block to
	// onDisk size in Finish() function.
	b.onDiskSize += vpLen
}

/*
Structure of Block.
+-------------------+---------------------+--------------------+--------------+------------------+
| Entry1            | Entry2              | Entry3             | Entry4       | Entry5           |
+-------------------+---------------------+--------------------+--------------+------------------+
| Entry6            | ...                 | ...                | ...          | EntryN           |
+-------------------+---------------------+--------------------+--------------+------------------+
| Block Meta(contains list of offsets used| Block Meta Size    | Block        | Checksum Size    |
| to perform binary search in the block)  | (4 Bytes)          | Checksum     | (4 Bytes)        |
+-----------------------------------------+--------------------+--------------+------------------+
*/
// In case the data is encrypted, the "IV" is added to the end of the block.
func (b *Builder) finishBlock() {
	if len(b.curBlock.entryOffsets) == 0 {
		return
	}

	// make a copy on the maxkey (previously taking only a reference on iterator key)
	b.curBlock.maxKey = make([]byte, int(b.lastKey.overlap)+len(b.lastKey.diff))
	copy(b.curBlock.maxKey, b.curBlock.baseKey[:b.lastKey.overlap])
	copy(b.curBlock.maxKey[b.lastKey.overlap:], b.lastKey.diff)

	// Append the entryOffsets and its length.
	b.append(y.U32SliceToBytes(b.curBlock.entryOffsets))
	b.append(y.U32ToBytes(uint32(len(b.curBlock.entryOffsets))))

	checksum := b.calculateChecksum(b.curBlock.data[:b.curBlock.end])

	// Append the block checksum and its length.
	b.append(checksum)
	b.append(y.U32ToBytes(uint32(len(checksum))))

	b.blockList = append(b.blockList, b.curBlock)
	b.uncompressedSize += uint32(b.curBlock.end) // seems no need to do this

	// Add length of baseKey (rounded to next multiple of 4 because of alignment).
	// Add another 40 Bytes, these additional 40 bytes consists of
	// 12 bytes of metadata of flatbuffer
	// 8 bytes for Key in flat buffer
	// 8 bytes for offset
	// 8 bytes for the len
	// 4 bytes for the size of slice while SliceAllocate
	b.lenOffsets += uint32(int(math.Ceil(float64(len(b.curBlock.baseKey))/4))*4) + 40

	// If compression/encryption is enabled, we need to send the block to the blockChan.
	if b.blockChan != nil {
		b.blockChan <- b.curBlock
	}
}

func (b *Builder) shouldFinishBlock(key []byte, value y.ValueStruct) bool {
	// If there is no entry till now, we will return false.
	if len(b.curBlock.entryOffsets) <= 0 {
		return false
	}

	// Integer overflow check for statements below.
	y.AssertTrue((uint32(len(b.curBlock.entryOffsets))+1)*4+4+8+4 < math.MaxUint32)
	// We should include current entry also in size, that's why +1 to len(b.entryOffsets).
	entriesOffsetsSize := uint32((len(b.curBlock.entryOffsets)+1)*4 +
		4 + // size of list
		8 + // Sum64 in checksum proto
		4) // checksum length
	estimatedSize := uint32(b.curBlock.end) + uint32(6 /*header size for entry*/) +
		uint32(len(key)) + uint32(value.EncodedSize()) + entriesOffsetsSize
	// Integer overflow check for table size.
	y.AssertTrue(uint64(b.curBlock.end)+uint64(estimatedSize) < math.MaxUint32)

	return estimatedSize > uint32(b.opts.BlockSize)
}

// AddStaleKey is same is Add function but it also increments the internal
// staleDataSize counter. This value will be used to prioritize this table for
// compaction.
func (b *Builder) AddStaleKey(key []byte, v y.ValueStruct, valueLen uint32) {
	// Rough estimate based on how much space it will occupy in the SST.
	b.staleDataSize += len(key) + len(v.Value) + 4 /* entry offset */ + 4 /* header size */
	b.addInternal(key, v, valueLen, true)
}

// Add adds a key-value pair to the block.
func (b *Builder) Add(key []byte, value y.ValueStruct, valueLen uint32) {
	b.addInternal(key, value, valueLen, false)
}

func (b *Builder) addInternal(key []byte, value y.ValueStruct, valueLen uint32, isStale bool) {
	if b.shouldFinishBlock(key, value) {
		if isStale {
			// This key will be added to tableIndex and it is stale.
			b.staleDataSize += len(key) + 4 /* len */ + 4 /* offset */
		}
		b.finishBlock()
		// Create a new block and start writing.
		b.curBlock = &bblock{
			data: b.alloc.Allocate(b.opts.BlockSize + padding),
		}
	}
	b.addHelper(key, value, valueLen)
}

func (b *Builder) appendFilter() {
	if b.opts.BloomFalsePositive <= 0 {
		return
	}
	if len(b.keyHashes) == 0 {
		return
	}
	// launch a goroutine to build the current filter
	f := &y.Filter{}
	b.filterList = append(b.filterList, f)
	b.filterWg.Add(1)
	go func(khs []uint32, f *y.Filter) {
		defer b.filterWg.Done()
		bits := y.BloomBitsPerKey(len(khs), b.opts.BloomFalsePositive)
		*f = y.NewFilter(khs, bits)
	}(b.keyHashes, f)
}

func (b *Builder) ReachedCapacity() bool {
	// If encryption/compression is enabled then use the compresssed size.
	sumBlockSizes := atomic.LoadUint32(&b.compressedSize)
	if b.opts.Compression == options.None {
		sumBlockSizes = b.uncompressedSize
	}
	blocksSize := sumBlockSizes + // actual length of current buffer
		uint32(len(b.curBlock.entryOffsets)*4) + // all entry offsets size
		4 + // count of all entry offsets
		8 + // checksum bytes
		4 // checksum length

	estimateSz := blocksSize +
		4 + // Index length
		b.lenOffsets

	return uint64(estimateSz) > b.opts.tableCapacity
}

// Finish finishes the table by appending the index.
/*
The table structure looks like
+---------+------------+-----------+---------------+
| Block 1 | Block 2    | Block 3   | Block 4       |
+---------+------------+-----------+---------------+
| Block 5 | Block 6    | Block ... | Block N       |
+---------+------------+-----------+---------------+
| Index   | Index Size | Checksum  | Checksum Size |
+---------+------------+-----------+---------------+
*/
// In case the data is encrypted, the "IV" is added to the end of the index.
func (b *Builder) Finish() []byte {
	bd := b.Done()
	buf := make([]byte, bd.Size)
	written := bd.Copy(buf)
	y.AssertTrue(written == len(buf))
	return buf
}

type buildData struct {
	filters   []*y.Filter
	blockList []*bblock
	index     []byte
	checksum  []byte
	dataSize  int
	Size      int
	alloc     *z.Allocator
}

func (bd *buildData) Copy(dst []byte) int {
	var written int
	for _, bl := range bd.blockList {
		written += copy(dst[written:], bl.data[:bl.end])
	}
	written += copy(dst[written:], bd.index)
	written += copy(dst[written:], y.U32ToBytes(uint32(len(bd.index))))

	written += copy(dst[written:], bd.checksum)
	written += copy(dst[written:], y.U32ToBytes(uint32(len(bd.checksum))))
	return written
}

func (bd *buildData) WriteToCSSF(mf io.CSSF) {
	if mf == nil {
		log.Println("no CSSF for build data to write to")
		return
	}
	for _, bl := range bd.blockList {
		mf.Write(bl.data[:bl.end])
	}
	mf.Write(bd.index)
	mf.Write(y.U32ToBytes(uint32(len(bd.index))))
	mf.Write(bd.checksum)
	mf.Write(y.U32ToBytes(uint32(len(bd.checksum))))
}

func (bd *buildData) WriteTo(name string, c io.CSSCli) (io.CSSF, error) {
	mf, err := c.OpenNewFile(name, bd.Size)
	if err != nil {
		return nil, y.Wrapf(err, "while creating table: %s", name)
	}
	bd.WriteToCSSF(mf)
	if err := mf.Sync(); err != nil {
		return nil, y.Wrapf(err, "while calling msync on %s", name)
	}
	return mf, nil
}

// separate finish block from Done, as set max key is still holding the iterator key reference.
func (b *Builder) Finalize(depleteBA bool) {
	b.finishBlock() // This will never start a new block.
	b.appendFilter()
	b.keyHashes = nil
	if !depleteBA {
		return
	}
}

// passing fname to construct blockid
func (b *Builder) Done() buildData {
	if b.blockChan != nil {
		close(b.blockChan)
	}
	// Wait for block handler to finish.
	b.wg.Wait()

	if len(b.blockList) == 0 {
		return buildData{}
	}
	bd := buildData{
		blockList: b.blockList,
		alloc:     b.alloc,
		filters:   b.filterList,
	}

	index, dataSize := b.buildIndex(b.filterList)

	checksum := b.calculateChecksum(index)

	bd.index = index
	bd.checksum = checksum
	bd.dataSize = int(dataSize)
	bd.Size = int(dataSize) + len(index) + len(checksum) + 4 + 4
	return bd
}

func (b *Builder) calculateChecksum(data []byte) []byte {
	// Build checksum for the index.
	checksum := pb.Checksum{
		// We chose to use CRC32 as the default option because
		// it performed better compared to xxHash64.
		// See the BenchmarkChecksum in table_test.go file
		// Size     =>   1024 B        2048 B
		// CRC32    => 63.7 ns/op     112 ns/op
		// xxHash64 => 87.5 ns/op     158 ns/op
		Sum:  y.CalculateChecksum(data, pb.Checksum_CRC32C),
		Algo: pb.Checksum_CRC32C,
	}

	// Write checksum to the file.
	chksum, err := proto.Marshal(&checksum)
	y.Check(err)
	// Write checksum size.
	return chksum
}

func (b *Builder) Opts() *Options {
	return b.opts
}

// compressData compresses the given data.
func (b *Builder) compressData(data []byte) ([]byte, error) {
	switch b.opts.Compression {
	case options.None:
		return data, nil
	case options.Snappy:
		sz := snappy.MaxEncodedLen(len(data))
		dst := b.alloc.Allocate(sz)
		return snappy.Encode(dst, data), nil
	case options.ZSTD:
		sz := y.ZSTDCompressBound(len(data))
		dst := b.alloc.Allocate(sz)
		return y.ZSTDCompress(dst, data, b.opts.ZSTDCompressionLevel)
	}
	return nil, errors.New("Unsupported compression type")
}

func (b *Builder) buildIndex(filters []*y.Filter) ([]byte, uint32) {
	builder := fbs.NewBuilder(3 << 20)

	// write filter
	var filterSize uint32
	var foEnd fbs.UOffsetT
	if len(filters) > 0 {
		b.filterWg.Wait()
		foList, fsize := b.writeFilters(builder, filters)
		filterSize = uint32(fsize)
		fb.TableIndexStartBloomFiltersVector(builder, len(foList))
		for i := len(foList) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(foList[i])
		}
		foEnd = builder.EndVector(len(foList))
	}

	boList, dataSize := b.writeBlockOffsets(builder)
	// Write block offset vector the the idxBuilder.
	fb.TableIndexStartOffsetsVector(builder, len(boList))

	// Write individual block offsets in reverse order to work around how Flatbuffers expects it.
	for i := len(boList) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(boList[i])
	}
	boEnd := builder.EndVector(len(boList))

	b.onDiskSize += filterSize
	b.onDiskSize += dataSize
	fb.TableIndexStart(builder)
	fb.TableIndexAddOffsets(builder, boEnd)
	fb.TableIndexAddBloomFilters(builder, foEnd)
	fb.TableIndexAddMaxVersion(builder, b.maxVersion)
	fb.TableIndexAddUncompressedSize(builder, b.uncompressedSize)
	fb.TableIndexAddOnDiskSize(builder, b.onDiskSize)
	fb.TableIndexAddStaleDataSize(builder, uint32(b.staleDataSize))
	builder.Finish(fb.TableIndexEnd(builder))

	buf := builder.FinishedBytes()
	index := fb.GetRootAsTableIndex(buf, 0)
	// Mutate the ondisk size to include the size of the index as well.
	y.AssertTrue(index.MutateOnDiskSize(index.OnDiskSize() + uint32(len(buf))))
	return buf, dataSize
}

// writeBlockOffsets writes all the blockOffets in b.offsets and returns the
// offsets for the newly written items.
func (b *Builder) writeBlockOffsets(builder *fbs.Builder) ([]fbs.UOffsetT, uint32) {
	var startOffset uint32
	var dataSize uint32
	var uoffs []fbs.UOffsetT
	for _, bl := range b.blockList {
		uoff := b.writeBlockOffset(builder, bl, startOffset)
		uoffs = append(uoffs, uoff)
		startOffset += uint32(bl.end)
	}
	dataSize = startOffset
	return uoffs, dataSize
}

// writeBlockOffset writes the given key,offset,len triple to the indexBuilder.
// It returns the offset of the newly written blockoffset.
func (b *Builder) writeBlockOffset(
	builder *fbs.Builder, bl *bblock, startOffset uint32) fbs.UOffsetT {
	// Write the key to the buffer.
	k := builder.CreateByteVector(bl.baseKey)
	id := builder.CreateString(bl.id)
	maxk := builder.CreateByteVector(bl.maxKey)

	// Build the blockOffset.
	fb.BlockOffsetStart(builder)
	fb.BlockOffsetAddMaxKey(builder, maxk)
	fb.BlockOffsetAddId(builder, id)
	fb.BlockOffsetAddKey(builder, k)
	fb.BlockOffsetAddOffset(builder, startOffset)
	fb.BlockOffsetAddLen(builder, uint32(bl.end))
	return fb.BlockOffsetEnd(builder)
}

func (b *Builder) writeFilters(builder *fbs.Builder, filters []*y.Filter) (uoffs []fbs.UOffsetT, filterSize int) {
	for _, f := range filters {
		uoff := b.writeFilter(builder, *f)
		uoffs = append(uoffs, uoff)
		filterSize += len(*f)
	}
	return
}

func (b *Builder) writeFilter(builder *fbs.Builder, filter y.Filter) fbs.UOffsetT {
	f := builder.CreateByteVector(filter)
	fb.FilterStart(builder)
	fb.FilterAddData(builder, f)
	return fb.FilterEnd(builder)
}
