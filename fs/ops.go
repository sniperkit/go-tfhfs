/*
 * Author: Markus Stenberg <fingon@iki.fi>
 *
 * Copyright (c) 2017 Markus Stenberg
 *
 * Created:       Thu Dec 28 12:52:43 2017 mstenber
 * Last modified: Fri Jan 12 13:49:28 2018 mstenber
 * Edit time:     370 min
 *
 */

package fs

import (
	"bytes"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/fingon/go-tfhfs/mlog"
	. "github.com/hanwen/go-fuse/fuse"
)

type fsOps struct {
	mu sync.Mutex
	fs *Fs
}

var _ RawFileSystem = &fsOps{}

func (self *fsOps) Init(server *Server) {
	self.fs.server = server
}

func (self *fsOps) String() string {
	return os.Args[0]
}

func (self *fsOps) SetDebug(dbg bool) {
	// TBD - do we need debug functionality someday?
}

func (self *fsOps) StatFs(input *InHeader, out *StatfsOut) Status {
	bsize := uint64(blockSize)
	out.Bsize = uint32(bsize)
	out.Frsize = uint32(bsize)
	avail := self.fs.storage.Backend.GetBytesAvailable() / bsize
	out.Bfree = avail
	out.Bavail = avail
	used := self.fs.storage.Backend.GetBytesUsed() / bsize
	total := used + avail
	out.Blocks = total
	return OK
}

func (self *fsOps) access(inode *inode, mode uint32, orOwn bool, ctx *Context) Status {
	if inode == nil {
		mlog.Printf2("fs/ops", "access: -does not exist")
		return ENOENT
	}
	meta := inode.Meta()
	if meta == nil {
		mlog.Printf2("fs/ops", "access: -meta does not exist")
		return ENOENT
	}
	if ctx.Uid == 0 {
		mlog.Printf2("fs/ops", "access: +root")
		return OK
	}
	perms := meta.StMode & 0x7
	if ctx.Uid == meta.StUid {
		if orOwn {
			mlog.Printf2("fs/ops", "access: +owner")
			return OK
		}
		perms |= (meta.StMode >> 6) & 0x7
	}
	if ctx.Gid == meta.StGid {
		perms |= (meta.StMode >> 3) & 0x7
	}
	if (perms & mode) == mode {
		mlog.Printf2("fs/ops", "access: - (%v %v)", perms, mode)
		return OK
	}
	mlog.Printf2("fs/ops", "access: - (%v %v)", perms, mode)
	return EACCES
}

// lookup gets child of a parent.
func (self *fsOps) lookup(parent *inode, name string, ctx *Context) (child *inode, code Status) {
	if parent == nil {
		code = ENOENT
		return
	}
	mlog.Printf2("fs/ops", "ops.lookup %v %s", parent.ino, name)
	code = self.access(parent, X_OK, false, ctx)
	if !code.Ok() {
		return
	}
	if !parent.IsDir() {
		code = ENOTDIR
		return
	}
	code = OK
	if name == "." {
		child = parent
	} else {
		child = parent.GetChildByName(name)
		if child == nil {
			code = ENOENT
		}
	}
	return
}

func (self *fsOps) Lookup(input *InHeader, name string, out *EntryOut) (code Status) {
	parent := self.fs.GetInode(input.NodeId)
	defer parent.Release()

	if parent == nil {
		return ENOENT
	}

	child, code := self.lookup(parent, name, &input.Context)
	defer child.Release()

	if code.Ok() {
		code = child.FillEntryOut(out)
	}
	return
}

func (self *fsOps) Forget(nodeID, nlookup uint64) {
	self.fs.GetInode(nodeID).Forget(nlookup)
}

func (self *fsOps) GetAttr(input *GetAttrIn, out *AttrOut) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	if inode == nil {
		return ENOENT
	}
	defer inode.Release()

	if input.Flags()&FUSE_GETATTR_FH != 0 {
		// fh := input.Fh()
		// ...
	}
	code = inode.FillAttrOut(out)
	return
}

func (self *fsOps) SetAttr(input *SetAttrIn, out *AttrOut) (code Status) {
	mlog.Printf2("fs/ops", "SetAttr")
	inode := self.fs.GetInode(input.NodeId)
	if inode == nil {
		mlog.Printf2("fs/ops", " no such file")
		return ENOENT
	}
	defer inode.Release()
	defer inode.metaWriteLock.Locked()()

	self.fs.Update(func(tr *fsTransaction) {
		meta := inode.Meta()
		newmeta := meta.InodeMetaData
		if input.Valid&(FATTR_ATIME|FATTR_MTIME|FATTR_ATIME_NOW|FATTR_MTIME_NOW|FATTR_CTIME) != 0 {
			var atime, ctime, mtime *time.Time

			now := time.Now()

			if input.Valid&FATTR_ATIME != 0 {
				if input.Valid&FATTR_ATIME_NOW != 0 {
					atime = &now
				} else {
					t := time.Unix(int64(input.Atime),
						int64(input.Atimensec))
					atime = &t
				}
			}

			if input.Valid&FATTR_MTIME != 0 {
				if input.Valid&FATTR_MTIME_NOW != 0 {
					mtime = &now
				} else {
					t := time.Unix(int64(input.Mtime),
						int64(input.Mtimensec))
					mtime = &t
				}
			}

			if input.Valid&FATTR_CTIME != 0 {
				t := time.Unix(int64(input.Ctime),
					int64(input.Ctimensec))
				ctime = &t
			}
			newmeta.setTimeValues(atime, ctime, mtime)
		}

		mode_filter := uint32(0)

		// FATTR_FH?
		if input.Valid&FATTR_UID != 0 && int32(input.Uid) != -1 && input.Uid != meta.StUid {
			newmeta.StUid = input.Uid
			if input.Context.Uid != 0 {
				mlog.Printf2("fs/ops", " non-root setting uid")
				code = EPERM
				// Non-root setting uid = bad.
				return
			}
			// On Linux/Darwin, this is expected behavior
			mode_filter |= syscall.S_ISUID | syscall.S_ISGID
		}
		// Eventually the Context.Gid checks could check
		// supplementary groups too
		if input.Valid&FATTR_GID != 0 && int32(input.Gid) != -1 && input.Gid != meta.StGid {
			newmeta.StGid = input.Gid
			if input.Context.Uid != 0 && input.Context.Uid != meta.StUid && input.Gid != input.Context.Gid {
				mlog.Printf2("fs/ops", " non-root setting gid")
				code = EPERM
				// Non-root setting uid = bad.
				return
			}
		}
		if input.Valid&FATTR_SIZE != 0 {
			newmeta.StSize = input.Size
		}

		oldmode := meta.StMode
		mode := oldmode

		if input.Context.Uid != 0 && input.Context.Uid != meta.StUid {
			// POSIXy weirdness - ignore setgid bit if we would fail
			mode_filter |= syscall.S_ISGID
		}

		if input.Valid&FATTR_MODE != 0 {
			mode = input.Mode & ^mode_filter
			// accept any mode bits, OS knows best?
			// (with OS X some relatively high bit modes are required,
			// e.g. 0100xxx seems to be needed at least for cp to work even)
			if input.Context.Uid != 0 && meta.StUid != input.Context.Uid && mode != oldmode {
				code = EPERM
				mlog.Printf(" non-root setting non-owned mode")
				return
			}

		}
		newmeta.StMode = mode & ^mode_filter

		if newmeta != meta.InodeMetaData {
			code = self.access(inode, W_OK, true, &input.Context)
			if !code.Ok() {
				mlog.Printf2("fs/ops", " inode not w-ok")
				code = EPERM
				return
			}
			if input.Valid&FATTR_SIZE != 0 {
				inode.SetMetaSizeInTransaction(meta, input.Size, tr)
			}

			newmeta.SetCTimeNow()
			meta.InodeMetaData = newmeta
			inode.SetMetaInTransaction(meta, tr)
		}
	})
	if code.Ok() {
		code = inode.FillAttrOut(out)
	}
	return
}

func (self *fsOps) Release(input *ReleaseIn) {
	self.fs.GetFileByFh(input.Fh).Release()
}

func (self *fsOps) ReleaseDir(input *ReleaseIn) {
	self.fs.GetFileByFh(input.Fh).Release()
}

func (self *fsOps) OpenDir(input *OpenIn, out *OpenOut) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, R_OK|X_OK, false, &input.Context)
	if !code.Ok() {
		return
	}

	meta := inode.Meta()
	meta.SetATimeNow()

	out.Fh = inode.GetFile(uint32(os.O_RDONLY)).fh
	return OK

}

func (self *fsOps) Open(input *OpenIn, out *OpenOut) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	mlog.Printf2("fs/ops", "ops.Open %v", input.NodeId)
	defer inode.Release()
	defer inode.metaWriteLock.Locked()()

	mode := uint32(R_OK)
	if input.Flags&uint32(os.O_WRONLY) == uint32(os.O_WRONLY) {
		mode = W_OK
	} else if input.Flags&O_ANYWRITE != 0 {
		mode |= W_OK
	}
	code = self.access(inode, mode, false, &input.Context)
	if !code.Ok() {
		return
	}

	self.fs.Update(func(tr *fsTransaction) {
		meta := inode.Meta()
		// No ATime for now
		if mode&W_OK != 0 {
			meta.SetMTimeNow()
		}

		if input.Flags&uint32(os.O_TRUNC) != 0 {
			inode.SetMetaSizeInTransaction(meta, 0, tr)
		}
		inode.SetMetaInTransaction(meta, tr)
	})

	out.Fh = inode.GetFile(input.Flags).fh
	return OK
}

func (self *fsOps) ReadDir(input *ReadIn, l *DirEntryList) Status {
	dir := self.fs.GetFileByFh(input.Fh)
	dir.SetPos(input.Offset)
	for dir.ReadDirEntry(l) {
	}
	return OK
}

func (self *fsOps) ReadDirPlus(input *ReadIn, l *DirEntryList) Status {
	dir := self.fs.GetFileByFh(input.Fh)
	dir.SetPos(input.Offset)
	for dir.ReadDirPlus(input, l) {
	}
	return OK
}

func (self *fsOps) Readlink(input *InHeader) (out []byte, code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, R_OK, false, &input.Context)
	if !code.Ok() {
		return
	}
	// Eventually check it is actually link?
	meta := inode.Meta()
	if meta == nil {
		code = ENOENT
		return
	}
	if (meta.StMode & S_IFLNK) == 0 {
		code = EINVAL
		return
	}
	out = meta.Data
	code = OK
	return
}

func (self *fsOps) create(input *InHeader, name string, meta *InodeMeta, allowReplace bool) (child *inode, code Status) {
	mlog.Printf2("fs/ops", " create %v", name)
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, W_OK|X_OK, false, &input.Context)
	if !code.Ok() {
		return
	}
	defer inode.metaWriteLock.Locked()()

	child = inode.GetChildByName(name)
	defer child.Release()
	if child != nil {
		if !allowReplace {
			code = Status(syscall.EEXIST)
			return
		}
		b := false
		code = self.unlinkInInode(inode, name, &b, &input.Context)
		if !code.Ok() {
			return
		}
	}

	child = self.fs.CreateInode()
	defer child.metaWriteLock.Locked()()
	self.fs.Update(func(tr *fsTransaction) {
		child.SetMetaInTransaction(meta, tr)
	})
	inode.AddChild(name, child)
	return
}

func (self *fsOps) Mkdir(input *MkdirIn, name string, out *EntryOut) (code Status) {
	var meta InodeMeta
	meta.SetMkdirIn(input)
	child, code := self.create(&input.InHeader, name, &meta, false)
	if !code.Ok() {
		return
	}
	defer child.Release()
	child.FillEntryOut(out)
	return OK
}

func (self *fsOps) unlinkInInode(inode *inode, name string, isdir *bool, ctx *Context) (code Status) {
	child, code := self.lookup(inode, name, ctx)
	defer child.Release()
	if !code.Ok() {
		return
	}

	code = self.access(inode, W_OK|X_OK, false, ctx)
	if !code.Ok() {
		return
	}
	if isdir != nil && *isdir != child.IsDir() {
		code = EPERM
		return
	}
	inode.RemoveChildByName(name)
	return OK
}

func (self *fsOps) unlink(input *InHeader, name string, isdir *bool) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()
	defer inode.metaWriteLock.Locked()()
	return self.unlinkInInode(inode, name, isdir, &input.Context)
}

func (self *fsOps) Unlink(input *InHeader, name string) (code Status) {
	mlog.Printf2("fs/ops", "ops.Unlink %s", name)
	b := false
	return self.unlink(input, name, &b)
}

func (self *fsOps) Rmdir(input *InHeader, name string) (code Status) {
	mlog.Printf2("fs/ops", "ops.Rmdir %s", name)
	b := true
	return self.unlink(input, name, &b)
}

func (self *fsOps) GetXAttrSize(input *InHeader, attr string) (size int, code Status) {
	b, code := self.GetXAttrData(input, attr)
	if !code.Ok() {
		return
	}
	return len(b), code
}

func (self *fsOps) GetXAttrData(input *InHeader, attr string) (data []byte, code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, R_OK, false, &input.Context)
	if !code.Ok() {
		return
	}

	return inode.GetXAttr(attr)
}

func (self *fsOps) SetXAttr(input *SetXAttrIn, attr string, data []byte) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, W_OK, true, &input.Context)
	if !code.Ok() {
		return
	}

	return inode.SetXAttr(attr, data)
}

func (self *fsOps) ListXAttr(input *InHeader) (data []byte, code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	defer inode.offsetMap.Locked(-1)()

	code = self.access(inode, R_OK, false, &input.Context)
	if !code.Ok() {
		return
	}
	b := bytes.NewBuffer([]byte{})
	inode.IterateSubTypeKeys(BST_XATTR,
		func(key blockKey) bool {
			b.Write([]byte(key.SubTypeData()))
			b.WriteByte(0)
			return true
		})
	data = b.Bytes()
	code = OK
	return
}

func (self *fsOps) RemoveXAttr(input *InHeader, attr string) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, W_OK, true, &input.Context)
	if !code.Ok() {
		return
	}
	return inode.RemoveXAttr(attr)
}

func (self *fsOps) Rename(input *RenameIn, oldName string, newName string) (code Status) {
	mlog.Printf2("fs/ops", "Rename")
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	code = self.access(inode, W_OK|X_OK, true, &input.Context)
	if !code.Ok() {
		mlog.Printf2("fs/ops", " no permissions")
		return
	}

	child, code := self.lookup(inode, oldName, &input.Context)
	defer child.Release()
	if !code.Ok() {
		mlog.Printf2("fs/ops", " no oldName")
		return
	}

	new_inode := self.fs.GetInode(input.Newdir)
	defer new_inode.Release()
	code = self.access(new_inode, W_OK|X_OK, true, &input.Context)
	if !code.Ok() {
		mlog.Printf2("fs/ops", " no write permission to newdir")
		return
	}

	new_child, code := self.lookup(new_inode, newName, &input.Context)
	defer new_child.Release()
	if code.Ok() {
		mlog.Printf2("fs/ops", " already exists, trying to unlink")
		ih := input.InHeader
		ih.NodeId = input.Newdir
		code = self.unlink(&ih, newName, nil)
		if !code.Ok() {
			mlog.Printf2("fs/ops", " unlink failed")
			return
		}
	}

	linkin := LinkIn{InHeader: input.InHeader,
		Oldnodeid: child.ino}
	linkin.NodeId = new_inode.ino
	code = self.Link(&linkin, newName, nil)
	if !code.Ok() {
		return
	}

	if oldName != newName || input.NodeId != input.Newdir {
		code = self.unlink(&input.InHeader, oldName, nil)
		if !code.Ok() {
			return
		}
	}
	return
}

func (self *fsOps) Link(input *LinkIn, name string, out *EntryOut) (code Status) {

	mlog.Printf2("fs/ops", "Link")
	inode := self.fs.GetInode(input.NodeId)
	if inode == nil {
		mlog.Printf2("fs/ops", " containing directory not found")
		return ENOENT
	}

	defer inode.Release()
	defer inode.metaWriteLock.Locked()()
	code = self.access(inode, W_OK|X_OK, true, &input.Context)
	if !code.Ok() {
		mlog.Printf2("fs/ops", " no access to containing directory")
		return
	}

	child, code := self.lookup(inode, name, &input.Context)
	if code.Ok() {
		mlog.Printf2("fs/ops", " existing child with name")
		defer child.Release()
		return Status(syscall.EEXIST)
	}

	child = self.fs.GetInode(input.Oldnodeid)
	if child == nil {
		mlog.Printf2("fs/ops", " original child %v not found", input.Oldnodeid)
		return ENOENT
	}
	defer child.Release()
	defer child.metaWriteLock.Locked()()
	inode.AddChild(name, child)
	child.FillEntryOut(out)
	return OK

}

func (self *fsOps) Access(input *AccessIn) (code Status) {
	inode := self.fs.GetInode(input.NodeId)
	defer inode.Release()

	return self.access(inode, input.Mask, false, &input.Context)
}

func (self *fsOps) Read(input *ReadIn, buf []byte) (ReadResult, Status) {
	// Check perm?
	file := self.fs.GetFileByFh(input.Fh)
	return file.Read(buf, input.Offset)
}

func (self *fsOps) Write(input *WriteIn, data []byte) (written uint32, code Status) {
	// Check perm?
	file := self.fs.GetFileByFh(input.Fh)
	return file.Write(data, input.Offset)
}

func (self *fsOps) Create(input *CreateIn, name string, out *CreateOut) (code Status) {
	mlog.Printf2("fs/ops", "ops.Create %s", name)
	// first create file
	var meta InodeMeta
	meta.SetCreateIn(input)
	child, code := self.create(&input.InHeader, name, &meta, input.Flags&uint32(os.O_EXCL) == 0)
	if !code.Ok() {
		return
	}
	defer child.Release()

	// then open the file.
	ih := input.InHeader
	ih.NodeId = child.ino
	var oo OpenOut
	code = self.Open(&OpenIn{InHeader: ih, Flags: input.Flags}, &oo)
	if !code.Ok() {
		return
	}
	child.FillEntryOut(&out.EntryOut)
	out.OpenOut = oo
	return OK
}

func (self *fsOps) Mknod(input *MknodIn, name string, out *EntryOut) (code Status) {
	var meta InodeMeta
	meta.SetMknodIn(input)
	child, code := self.create(&input.InHeader, name, &meta, false)
	if !code.Ok() {
		return
	}
	defer child.Release()
	child.FillEntryOut(out)
	return OK
}

func (self *fsOps) Symlink(input *InHeader, pointedTo string, linkName string, out *EntryOut) (code Status) {
	meta := InodeMeta{InodeMetaData: InodeMetaData{StUid: input.Uid,
		StGid:  input.Gid,
		StMode: S_IFLNK | 0777,
		StSize: uint64(len(pointedTo)),
	},
		Data: []byte(pointedTo)}
	child, code := self.create(input, linkName, &meta, false)
	if !code.Ok() {
		return
	}
	defer child.Release()
	child.FillEntryOut(out)
	return OK
}

func (self *fsOps) Flush(input *FlushIn) Status {
	// TBD
	return ENOSYS
}

func (self *fsOps) Flock(input *FlockIn, flags int) Status {
	// TBD
	return ENOSYS
}

func (self *fsOps) Fsync(input *FsyncIn) (code Status) {
	// TBD
	return ENOSYS
}

func (self *fsOps) FsyncDir(input *FsyncIn) (code Status) {
	// TBD
	return ENOSYS
}

func (self *fsOps) Fallocate(in *FallocateIn) (code Status) {
	// TBD
	return ENOSYS
}
