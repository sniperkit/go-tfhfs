/*
 * Author: Markus Stenberg <fingon@iki.fi>
 *
 * Copyright (c) 2017 Markus Stenberg
 *
 * Created:       Thu Dec 14 19:10:02 2017 mstenber
 * Last modified: Sat Dec 23 15:04:26 2017 mstenber
 * Edit time:     175 min
 *
 */

package storage

import (
	"sort"
	"time"
	"unsafe"

	"github.com/fingon/go-tfhfs/tfhfs_proto"
)

// Block is externally usable read-only structure that is handled
// using BlockStorer interface. Notably changes to 'Id' and 'Data' are
// not allowed, and Status should be mutated only via
// UpdateBlockStatus call of BlockStorer.
type Block struct {
	// Id contains identity of the block, derived from Data if not
	// set.
	Id string

	// Actually encoded plaintext data (if available; GetData()
	// should be used to get it always)
	Data string

	// Node is the actual btree node encoded within this
	// block. Used to derive Data as needed.
	//Node *TreeNode

	// Status describes the desired behavior of sub-references and
	// availability of data of a block.
	Status tfhfs_proto.BlockStatus

	// RefCount is the non-negative number of references to a
	// block _on disk_ (or what should be on disk).
	refCount int

	// Storage this is stored on, if any
	storage *Storage

	// Stored version of the block, if any. Set only if something
	// has changed locally.
	stored *Block

	// Time info if any
	t time.Time
}

func (self *Block) GetData() string {
	if self.Data == "" {
		self.Data = self.storage.Backend.GetBlockData(self)
	}
	return self.Data
}

func (self *Block) flush() int {
	// self.stored MUST be set, otherwise we wouldn't be dirty!
	ops := 0
	if self.stored.refCount == 0 {
		if self.refCount > 0 {
			self.storage.Backend.StoreBlock(self)
			ops = ops + 1
		} else {
			ops = ops + self.storage.updateBlock(self)
		}
	} else {
		if self.stored.Status != self.Status {
			self.flushStatus()
			ops = ops + 1
		}
		ops = ops + self.storage.updateBlock(self)
	}
	self.stored = nil
	return ops
}

func (self *Block) flushStatus() {
	// self.stored.status != self.status
	if self.Status == tfhfs_proto.BlockStatus_MISSING {
		// old type = NORMAL
		return
	}
	if self.Status == tfhfs_proto.BlockStatus_WANT_WEAK {
		// old type = WEAK
		return
	}
	data := self.GetData()
	self.storage.updateBlockDataDependencies(data, true, self.Status)
	self.storage.updateBlockDataDependencies(data, false, self.stored.Status)

}

func (self *Block) getCacheSize() int {
	s := int(unsafe.Sizeof(*self))
	return s + len(self.Id) + len(self.Data)
}

func (self *Block) setRefCount(count int) {
	self.markDirty()
	self.refCount = count
}

func (self *Block) setStatus(st tfhfs_proto.BlockStatus) {
	self.markDirty()
	self.Status = st

}

func (self *Block) markDirty() {
	if self.stored != nil {
		return
	}
	self.stored = &Block{Id: self.Id, Data: self.Data, Status: self.Status,
		refCount: self.refCount}
	// Add to dirty block list
	self.storage.dirty_bid2block[self.Id] = self
}

// BlockBackend is the shadow behind the throne; it actually
// handles the low-level operations of blocks.
type BlockBackend interface {
	// DeleteBlock removes block from storage, and it MUST exist.
	DeleteBlock(b *Block)

	// GetBlockData retrieves lazily (if need be) block data
	GetBlockData(b *Block) string

	// GetBlockById returns block by id or nil.
	GetBlockById(id string) *Block

	// GetBlockIdByName returns block id mapped to particular name.
	GetBlockIdByName(name string) string

	// GetBytesAvailable returns number of bytes available.
	GetBytesAvailable() int

	// GetBytesUsed returns number of bytes used.
	GetBytesUsed() int

	// SetBlockIdName sets the logical name to map to particular block id.
	SetNameToBlockId(name, block_id string)

	// StoreBlock adds new block to storage. It MUST NOT exist.
	StoreBlock(b *Block)

	// UpdateBlock updates block metadata in storage. It MUST exist.
	UpdateBlock(b *Block) int
}

type BlockIterateReferencesCallback func(string, func(string))
type BlockHasExternalReferencesCallback func(string) bool

// Storage is essentially DelayedStorage of Python prototype; it has
// dirty tracking of blocks, delayed flush to BlockBackend, and
// caching of data.
type oldNewStruct struct{ old_value, new_value string }
type Storage struct {
	Backend                       BlockBackend
	IterateReferencesCallback     BlockIterateReferencesCallback
	HasExternalReferencesCallback BlockHasExternalReferencesCallback

	// Map of block id => block for dirty blocks.
	dirty_bid2block map[string]*Block

	// Blocks that have refcnt0 but BlockHasExternalReferencesCallback has
	// claimed they should still be around
	referenced_refcnt0_blocks map[string]*Block

	// Stuff below here is ~DelayedStorage
	names                          map[string]*oldNewStruct
	cache_bid2block                map[string]*Block
	cache_size, maximum_cache_size int
}

// Init sets up the default values to be usable
func (self Storage) Init() *Storage {
	self.dirty_bid2block = make(map[string]*Block)
	self.cache_bid2block = make(map[string]*Block)
	self.names = make(map[string]*oldNewStruct)
	return &self
}

func (self *Storage) gocBlockById(id string) *Block {
	b, ok := self.cache_bid2block[id]
	if !ok {
		b = self.getBlockById(id)
		if b == nil {
			b = &Block{Id: id, storage: self}
		}
		self.cache_size += b.getCacheSize()
		self.cache_bid2block[id] = b
	}
	b.t = time.Now()
	return b
}

func (self *Storage) updateBlockDataDependencies(data string, add bool, st tfhfs_proto.BlockStatus) {
	// No sub-references
	if st >= tfhfs_proto.BlockStatus_WANT_NORMAL {
		return
	}
	if self.IterateReferencesCallback == nil {
		return
	}
	self.IterateReferencesCallback(data, func(id string) {
		if add {
			self.ReferBlockId(id)
		} else {
			self.ReleaseBlockId(id)
		}
	})
}

func (self *Storage) blockValid(b *Block) bool {
	if b == nil {
		return false
	}
	if b.refCount == 0 {
		if self.HasExternalReferencesCallback != nil && self.HasExternalReferencesCallback(b.Id) {
			return true
		}
		return false
	}
	return true
}

// getBlockById is the old Storage version; GetBlockIdBy is the external one
func (self *Storage) getBlockById(id string) *Block {
	b := self.dirty_bid2block[id]
	if self.blockValid(b) {
		return b
	}
	if self.referenced_refcnt0_blocks != nil {
		b := self.referenced_refcnt0_blocks[id]
		if self.blockValid(b) {
			return b
		}
	}
	return self.Backend.GetBlockById(id)
}

func (self *Storage) ReferBlockId(id string) {
	b, ok := self.GetBlockById(id)
	if !ok {
		panic("block id disappeared")
	}
	b.setRefCount(b.refCount + 1)
}

func (self *Storage) GetBlockById(id string) (*Block, bool) {
	b := self.gocBlockById(id)
	if self.blockValid(b) {
		return b, true
	}
	return nil, false
}

// ReleaseBlockId releases a block, and returns whether the block is
// still usable or not.
func (self *Storage) ReleaseBlockId(id string) bool {
	b, ok := self.GetBlockById(id)
	if !ok {
		panic("block id disappeared")
	}
	b.setRefCount(b.refCount - 1)
	if b.refCount == 0 {
		if self.deleteBlockIfNoExtRef(b) {
			return false
		}
	}
	return true
}

func (self *Storage) deleteBlockWithDeps(b *Block) bool {
	self.updateBlockDataDependencies(b.GetData(), false, b.Status)
	self.Backend.DeleteBlock(b)
	self.deleteCachedBlock(b)
	return true
}

func (self *Storage) deleteBlockIfNoExtRef(b *Block) bool {
	if self.HasExternalReferencesCallback != nil && self.HasExternalReferencesCallback(b.Id) {
		if self.referenced_refcnt0_blocks == nil {
			self.referenced_refcnt0_blocks = make(map[string]*Block)
		}
		self.referenced_refcnt0_blocks[b.Id] = b
		return false
	}
	if self.referenced_refcnt0_blocks != nil {
		delete(self.referenced_refcnt0_blocks, b.Id)
	}
	return self.deleteBlockWithDeps(b)
}

func (self *Storage) Flush() int {
	oops := -1
	ops := 0
	// _flush_names in Python prototype
	for k, v := range self.names {
		if v.old_value != v.new_value {
			self.Backend.SetNameToBlockId(k, v.new_value)
			v.old_value = v.new_value
			ops = ops + 1
		}
	}
	// Main flush in Python prototype; handles deletion
	for ops != oops {
		oops = ops
		s := self.referenced_refcnt0_blocks
		if s == nil {
			break
		}
		self.referenced_refcnt0_blocks = nil
		for _, v := range s {
			if v.refCount == 0 && self.deleteBlockIfNoExtRef(v) {
				ops = ops + 1
			}
		}
	}

	// flush_dirty_stored_blocks in Python
	for len(self.dirty_bid2block) > 0 {
		dirty := self.dirty_bid2block
		self.dirty_bid2block = make(map[string]*Block)
		nonzero_blocks := make([]*Block, 0)
		for _, b := range dirty {
			if b.refCount == 0 {
				ops = ops + b.flush()
			} else {
				nonzero_blocks = append(nonzero_blocks, b)
			}
		}
		for _, b := range nonzero_blocks {
			if b.refCount > 0 {
				ops = ops + b.flush()
			} else {
				// populate for subsequent round
				self.dirty_bid2block[b.Id] = b
			}
		}
	}

	// end of flush in DelayedStorage in Python prototype
	if self.maximum_cache_size > 0 && self.cache_size > self.maximum_cache_size {
		self.shrinkCache()
	}
	return ops
}

func (self *Storage) shrinkCache() {
	n := len(self.cache_bid2block)
	arr := make([]*Block, n)
	i := 0
	for _, v := range self.cache_bid2block {
		arr[i] = v
		i = i + 1
	}
	sort.Slice(arr, func(i, j int) bool {
		return arr[i].t.After(arr[j].t)
	})
	i = 0
	goal := self.maximum_cache_size * 3 / 4
	for i < n && self.cache_size > goal {
		self.deleteCachedBlock(arr[i])
	}

}

func (self *Storage) deleteCachedBlock(b *Block) {
	delete(self.cache_bid2block, b.Id)
	self.cache_size = self.cache_size - b.getCacheSize()
	if b.stored.refCount == 0 {
		// Locally stored, never hit disk, but references did
		self.updateBlockDataDependencies(b.Data, false, b.Status)
	}
}

func (self *Storage) updateBlock(b *Block) int {
	if b.refCount == 0 {
		if b.stored.refCount == 0 {
			self.deleteCachedBlock(b)
			return 0
		}
		if self.deleteBlockIfNoExtRef(b) {
			return 1
		}
	}
	return self.Backend.UpdateBlock(b)
}

func (self *Storage) StoreBlock(id string, data string, status tfhfs_proto.BlockStatus) *Block {
	b := self.gocBlockById(id)
	b.setRefCount(1)
	b.setStatus(status)
	b.Data = data
	self.cache_size = self.cache_size + b.getCacheSize()
	self.updateBlockDataDependencies(data, true, status)
	return b

}

func (self *Storage) getName(name string) *oldNewStruct {
	n, ok := self.names[name]
	if ok {
		return n
	}
	id := self.Backend.GetBlockIdByName(name)
	n = &oldNewStruct{old_value: id, new_value: id}
	self.names[name] = n
	return n
}

func (self *Storage) GetBlockIdByName(name string) string {
	return self.getName(name).new_value
}

func (self *Storage) SetNameToBlockId(name, block_id string) {
	self.getName(name).new_value = block_id
}

func (self *Storage) ReferOrStoreBlock(id, data string) *Block {
	b, ok := self.GetBlockById(id)
	if ok {
		self.ReferBlockId(id)
		return b
	}
	return self.StoreBlock(id, data, tfhfs_proto.BlockStatus_NORMAL)
}
