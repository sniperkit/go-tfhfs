/*
 * Author: Markus Stenberg <fingon@iki.fi>
 *
 * Copyright (c) 2018 Markus Stenberg
 *
 * Created:       Thu Jan 11 08:32:34 2018 mstenber
 * Last modified: Thu Feb  1 22:20:02 2018 mstenber
 * Edit time:     65 min
 *
 */

package storage

import (
	"log"

	"github.com/fingon/go-tfhfs/mlog"
	"github.com/fingon/go-tfhfs/util"
)

type jobType int

const (
	jobFlush jobType = iota
	jobGetBlockById
	jobGetBlockIdByName
	jobSetNameToBlockId
	jobSetStorageBlockStatus
	jobReferOrStoreBlock            // ReferOrStoreBlock, ReferOrStoreBlock0
	jobUpdateBlockIdRefCount        // ReferBlockId, ReleaseBlockId
	jobUpdateBlockIdStorageRefCount // ReleaseStorageBlockId
	jobStoreBlock                   // StoreBlock, StoreBlock0
	jobQuit
)

type jobOut struct {
	sb *StorageBlock
	id string
	ok bool
}

type jobIn struct {
	// see job* above
	jobType jobType

	sb *StorageBlock

	// in jobReferOrStoreBlock, jobUpdateBlockIdRefCount, jobStoreBlock
	count int32

	// block id
	id string

	// block name
	name string

	// block data
	data []byte

	// dependencies (if any)
	deps *util.StringList

	status BlockStatus

	out chan *jobOut
}

func (self *Storage) run() {
	for job := range self.jobChannel {
		self.jobCounts[job.jobType] = self.jobCounts[job.jobType] + 1
		mlog.Printf2("storage/storagejob", "st.run job %v", job.jobType)
		switch job.jobType {
		case jobQuit:
			job.out <- nil
			return
		case jobFlush:
			self.flush()
			job.out <- nil
		case jobGetBlockById:
			b := self.getBlockById(job.sb.id)
			job.sb.setBlock(b)
		case jobGetBlockIdByName:
			job.out <- &jobOut{id: self.getName(job.name).newValue}
		case jobReferOrStoreBlock:
			b := self.getBlockById(job.sb.id)
			if b != nil {
				b.addRefCount(job.count)
				job.sb.setBlock(b)
				continue
			}
			mlog.Printf2("storage/storagejob", "fallthrough to storing block")
			fallthrough
		case jobStoreBlock:
			b := &Block{Id: job.sb.id,
				storage: self,
				deps:    job.deps,
			}
			//nd := make([]byte, len(job.data))
			//mlog.Printf2("storage/storagejob", "allocated size:%d", len(job.data))
			//copy(nd, job.data)
			//b.Data.Set(&nd)
			b.Data.Set(&job.data)
			self.blocks[job.sb.id] = b
			b.Status = job.status
			b.addRefCount(job.count)
			job.sb.setBlock(b)
		case jobUpdateBlockIdRefCount:
			b := self.getBlockById(job.id)
			if b == nil {
				log.Panicf("block id %x disappeared", job.id)
			}
			b.addRefCount(job.count)
		case jobUpdateBlockIdStorageRefCount:
			b := self.getBlockById(job.id)
			if b == nil {
				log.Panicf("block id %x disappeared", job.id)
			}
			// Now handled directly within StorageBlock
			b.addStorageRefCount(job.count)
		case jobSetNameToBlockId:
			self.setNameToBlockId(job.name, job.id)
		case jobSetStorageBlockStatus:
			jo := &jobOut{ok: job.sb.block.Get().setStatus(job.status)}
			job.out <- jo
		default:
			log.Panicf("Unknown job type: %d", job.jobType)
		}
		mlog.Printf2("storage/storagejob", " st.run job done")
	}
}

func (self *Storage) Flush() {
	out := make(chan *jobOut, 1)
	self.jobChannel <- &jobIn{jobType: jobFlush, out: out}
	<-out
}

func (self *Storage) GetBlockById(id string) *StorageBlock {
	sb := newStorageBlock(id)
	self.jobChannel <- &jobIn{jobType: jobGetBlockById,
		sb: sb,
	}
	b := sb.block.Get()
	if b == nil {
		return nil
	}
	return sb
}

func (self *Storage) GetBlockIdByName(name string) string {
	out := make(chan *jobOut, 1)
	self.jobChannel <- &jobIn{jobType: jobGetBlockIdByName, out: out,
		name: name,
	}
	jr := <-out
	return jr.id
}

func (self *Storage) storeBlockInternal(jobType jobType, id string, status BlockStatus, data []byte, deps *util.StringList, count int32) *StorageBlock {
	sb := newStorageBlock(id)
	self.jobChannel <- &jobIn{jobType: jobType,
		sb: sb, data: data, deps: deps, count: count, status: status,
	}
	return sb
}

func (self *Storage) ReferOrStoreBlock(id string, status BlockStatus, data []byte) *StorageBlock {
	return self.storeBlockInternal(jobReferOrStoreBlock, id, status, data, nil, 1)
}

func (self *Storage) ReferOrStoreBlock0(id string, status BlockStatus, data []byte, deps *util.StringList) *StorageBlock {
	return self.storeBlockInternal(jobReferOrStoreBlock, id, status, data, deps, 0)
}

func (self *Storage) ReferBlockId(id string) {
	self.jobChannel <- &jobIn{jobType: jobUpdateBlockIdRefCount,
		id: id, count: 1,
	}
}

func (self *Storage) ReferStorageBlockId(id string) {
	mlog.Printf2("storage/storagejob", "ReferStorageBlockId %x", id)
	self.jobChannel <- &jobIn{jobType: jobUpdateBlockIdStorageRefCount,
		id: id, count: 1,
	}
}

func (self *Storage) ReleaseBlockId(id string) {
	self.jobChannel <- &jobIn{jobType: jobUpdateBlockIdRefCount,
		id: id, count: -1,
	}
}

func (self *Storage) ReleaseStorageBlockId(id string) {
	mlog.Printf2("storage/storagejob", "ReleaseStorageBlockId %x", id)
	self.jobChannel <- &jobIn{jobType: jobUpdateBlockIdStorageRefCount,
		id: id, count: -1,
	}
}

func (self *Storage) SetNameToBlockId(name, block_id string) {
	self.jobChannel <- &jobIn{jobType: jobSetNameToBlockId,
		id: block_id, name: name,
	}
}

func (self *Storage) StoreBlock(id string, status BlockStatus, data []byte) *StorageBlock {
	return self.storeBlockInternal(jobStoreBlock, id, status, data, nil, 1)
}

func (self *Storage) StoreBlock0(id string, status BlockStatus, data []byte) *StorageBlock {
	return self.storeBlockInternal(jobStoreBlock, id, status, data, nil, 0)
}

func (self *Storage) setStorageBlockStatus(sb *StorageBlock, status BlockStatus) bool {

	out := make(chan *jobOut, 1)
	self.jobChannel <- &jobIn{jobType: jobSetStorageBlockStatus, out: out,
		sb: sb, status: status,
	}
	jr := <-out
	return jr.ok
}
