/*
 * Author: Markus Stenberg <fingon@iki.fi>
 *
 * Copyright (c) 2017 Markus Stenberg
 *
 * Created:       Fri Dec 29 08:21:32 2017 mstenber
 * Last modified: Fri Dec 29 13:00:44 2017 mstenber
 * Edit time:     102 min
 *
 */

package fs

import (
	"encoding/binary"
	"log"
	"math/rand"
	"time"

	"github.com/fingon/go-tfhfs/ibtree"
	"github.com/fingon/go-tfhfs/util"
	"github.com/hanwen/go-fuse/fuse"
)

type InodeFile struct {
	inode   *Inode
	fh      uint64
	pos     uint64
	lastKey *BlockKey
}

func (self *InodeFile) ReadNextInode() (inode *Inode, name string) {
	// dentry at lastName (if set) or pos (if not set);
	// return true if reading was successful (and pos got advanced)
	tr := self.Fs().GetTransaction()
	lkp := self.lastKey
	if lkp == nil {
		lastKey := NewBlockKey(self.inode.ino, BST_DIR_NAME2INODE, "")
		for i := uint64(0); i < self.pos; i++ {
			nkeyp := tr.NextKey(ibtree.IBKey(lastKey))
			if nkeyp == nil {
				return nil, ""
			}
			nkey := BlockKey(*nkeyp)
			if nkey.Ino() != lastKey.Ino() || nkey.SubType() != lastKey.SubType() {
				return nil, ""
			}
			lastKey = nkey
		}
		lkp = &lastKey
	}
	nkeyp := tr.NextKey(ibtree.IBKey(*lkp))
	if nkeyp == nil {
		return nil, ""
	}
	nkey := BlockKey(*nkeyp)
	if nkey.Ino() != self.lastKey.Ino() || nkey.SubType() != self.lastKey.SubType() {
		return nil, ""
	}
	inop := tr.Get(*nkeyp)
	ino := binary.BigEndian.Uint64([]byte(*inop))
	name = string(nkey.SubTypeData()[filenameHashSize:])
	inode = self.inode.tracker.GetInode(ino)
	return
}

func (self *InodeFile) ReadDirEntry(l *fuse.DirEntryList) bool {
	inode, name := self.ReadNextInode()
	defer inode.Release()
	if inode == nil {
		return false
	}
	defer inode.Release()
	meta := inode.Meta()
	e := fuse.DirEntry{Mode: meta.StMode, Name: name, Ino: inode.ino}
	ok, _ := l.AddDirEntry(e)
	if ok {
		self.pos++
		nkey := NewBlockKeyDirFilename(inode.ino, name)
		self.lastKey = &nkey
	}
	return ok
}

func (self *InodeFile) ReadDirPlus(input *fuse.ReadIn, l *fuse.DirEntryList) bool {
	inode, name := self.ReadNextInode()
	defer inode.Release()
	if inode == nil {
		return false
	}
	defer inode.Release()
	meta := inode.Meta()
	e := fuse.DirEntry{Mode: meta.StMode, Name: name, Ino: inode.ino}
	entry, _ := l.AddDirLookupEntry(e)
	if entry == nil {
		return false
	}
	*entry = fuse.EntryOut{}
	self.Fs().Lookup(&input.InHeader, name, entry)

	// Move on with things
	self.pos++
	nkey := NewBlockKeyDirFilename(inode.ino, name)
	self.lastKey = &nkey
	return true
}

func (self *InodeFile) Fs() *Fs {
	return self.inode.Fs()
}

func (self *InodeFile) Release() {
	delete(self.inode.tracker.fh2ifile, self.fh)
	self.inode.Release()
}

func (self *InodeFile) SetPos(pos uint64) {
	if self.pos == pos {
		return
	}
	self.pos = pos
	// TBD - does this need something else too?
	self.lastKey = nil
}

type Inode struct {
	ino     uint64
	tracker *InodeTracker
	refcnt  uint64
	meta    *InodeMeta
}

func (self *Inode) AddChild(name string, child *Inode) {
	tr := self.Fs().GetTransaction()
	k := NewBlockKeyDirFilename(self.ino, name)
	rk := NewBlockKeyReverseDirFilename(child.ino, self.ino, name)
	tr.Set(ibtree.IBKey(k), string(util.Uint64Bytes(child.ino)))
	tr.Set(ibtree.IBKey(rk), "")
	meta := child.Meta()
	meta.StNlink++
	child.SetMeta(meta)

	meta = self.Meta()
	meta.Nchildren++
	self.SetMeta(meta)

	self.Fs().CommitTransaction(tr)
}

func (self *Inode) Fs() *Fs {
	return self.tracker.fs
}

func (self *Inode) FillAttr(out *fuse.Attr) fuse.Status {
	// EntryOut.Attr
	meta := self.Meta()
	if meta == nil {
		return fuse.ENOENT
	}
	out.Size = meta.StSize
	out.Blocks = meta.StSize / blockSize
	out.Atime = meta.StAtimeNs
	out.Ctime = meta.StCtimeNs
	out.Mtime = meta.StMtimeNs
	out.Mode = meta.StMode
	out.Nlink = meta.StNlink
	// TBD rdev?
	// EntryOut.Attr.Owner
	out.Uid = meta.StUid
	out.Gid = meta.StGid
	return fuse.OK
}

func (self *Inode) FillAttrOut(out *fuse.AttrOut) fuse.Status {
	out.AttrValid = attrValidity
	out.AttrValidNsec = 0
	if out.Nlink == 0 {
		out.Nlink = 1
		// original hanwen's work does this, is this really
		// necessary? (allegedly new kernels have issues with
		// nlink=0 + link)
	}
	return self.FillAttr(&out.Attr)
}

func (self *Inode) FillEntryOut(out *fuse.EntryOut) fuse.Status {
	// EntryOut
	out.Ino = self.ino
	out.NodeId = self.ino
	out.Generation = 0
	out.EntryValid = entryValidity
	out.AttrValid = attrValidity
	out.EntryValidNsec = 0
	out.AttrValidNsec = 0

	return self.FillAttr(&out.Attr)
}

func (self *Inode) GetChildByName(name string) *Inode {
	k := NewBlockKeyDirFilename(self.ino, name)
	tr := self.Fs().GetTransaction()
	v := tr.Get(ibtree.IBKey(k))
	if v == nil {
		return nil
	}
	ino := binary.BigEndian.Uint64([]byte(*v))
	return self.tracker.GetInode(ino)
}

func (self *Inode) GetFile() *InodeFile {
	file := &InodeFile{inode: self}
	self.tracker.AddFile(file)
	self.Refer()
	return file
}

func (self *Inode) SetTimes(atime *time.Time, mtime *time.Time) fuse.Status {
	meta := self.Meta()
	if meta == nil {
		return fuse.ENOENT
	}
	if atime != nil {
		meta.StAtimeNs = uint64(atime.UnixNano())
	}
	if mtime != nil {
		meta.StMtimeNs = uint64(mtime.UnixNano())
	}
	return fuse.OK
}

func (self *Inode) UpdateAtime() {
	now := time.Now()
	self.SetTimes(&now, nil)
}

func (self *Inode) UpdateMtime() {
	now := time.Now()
	self.SetTimes(&now, &now)
}

func (self *Inode) IsDir() bool {
	meta := self.Meta()
	return meta != nil && (meta.StMode&fuse.S_IFDIR) != 0
}

func (self *Inode) IsFile() bool {
	meta := self.Meta()
	return meta != nil && (meta.StMode&fuse.S_IFREG) != 0
}

func (self *Inode) IsLink() bool {
	meta := self.Meta()
	return meta != nil && (meta.StMode&fuse.S_IFLNK) != 0
}

func (self *Inode) Refer() {
	self.refcnt++
}

func (self *Inode) Forget(refcnt uint64) {
	self.refcnt -= refcnt
	if self.refcnt == 0 {
		// TBD if there's something else that should be done?
		delete(self.tracker.ino2inode, self.ino)
	}
}

func (self *Inode) Release() {
	if self == nil {
		return
	}
	self.Forget(1)
}

func (self *Inode) RemoveChildByName(name string) {
	child := self.GetChildByName(name)
	defer child.Release()
	if child == nil {
		return
	}
	tr := self.Fs().GetTransaction()
	k := NewBlockKeyDirFilename(self.ino, name)
	rk := NewBlockKeyReverseDirFilename(child.ino, self.ino, name)
	tr.Delete(ibtree.IBKey(k))
	tr.Delete(ibtree.IBKey(rk))
	meta := child.Meta()
	meta.StNlink--
	child.SetMeta(meta)

	meta = self.Meta()
	meta.Nchildren--
	self.SetMeta(meta)

	self.Fs().CommitTransaction(tr)
}

// Meta caches the current metadata for particular inode.
// It is valid for the duration of the inode, within validity period anyway.
func (self *Inode) Meta() *InodeMeta {
	if self.meta == nil {
		k := NewBlockKey(self.ino, BST_META, "")
		tr := self.Fs().GetTransaction()
		v := tr.Get(ibtree.IBKey(k))
		if v == nil {
			return nil
		}
		var m InodeMeta
		_, err := m.UnmarshalMsg([]byte(*v))
		if err != nil {
			log.Panic(err)
		}
		self.meta = &m
	}
	return self.meta
}

func (self *Inode) SetMeta(meta *InodeMeta) {
	k := NewBlockKey(self.ino, BST_META, "")
	tr := self.Fs().GetTransaction()
	b, err := meta.MarshalMsg(nil)
	if err != nil {
		log.Panic(err)
	}
	tr.Set(ibtree.IBKey(k), string(b))
	self.Fs().CommitTransaction(tr)
	self.meta = meta
}

type InodeTracker struct {
	ino2inode map[uint64]*Inode
	fh2ifile  map[uint64]*InodeFile
	fs        *Fs
	nextFh    uint64
}

func (self *InodeTracker) Init(fs *Fs) {
	self.ino2inode = make(map[uint64]*Inode)
	self.fh2ifile = make(map[uint64]*InodeFile)
	self.fs = fs
	self.nextFh = 1
}

func (self *InodeTracker) AddFile(file *InodeFile) {
	self.nextFh++
	fh := self.nextFh
	file.fh = fh
	self.fh2ifile[fh] = file
}

func (self *InodeTracker) getInode(ino uint64, create bool) *Inode {
	n := self.ino2inode[ino]
	if n == nil {
		if create {
			n = &Inode{ino: ino, tracker: self}
			self.ino2inode[ino] = n
		}
	}
	if n != nil {
		n.refcnt++
	}
	return n
}

func (self *InodeTracker) GetInode(ino uint64) *Inode {
	// TBD if create=false is ever useful..
	return self.getInode(ino, true)
}

func (self *InodeTracker) GetFile(fh uint64) *InodeFile {
	return self.fh2ifile[fh]
}

func (self *InodeTracker) CreateInode() *Inode {
	for {
		ino := rand.Uint64()
		if self.ino2inode[ino] != nil {
			continue
		}

		// Potentially interesting. See if it is on disk.
		inode := self.GetInode(ino)
		if inode.Meta() != nil {
			inode.Release()
			continue
		}

		// We have fresh inode for ourselves!
		return inode
	}
}

// Misc utility stuff

func (self *InodeMeta) SetMkdirIn(input *fuse.MkdirIn) {
	self.StUid = input.Uid
	self.StGid = input.Gid
	self.StMode = input.Mode
	// TBD: Umask?

}
