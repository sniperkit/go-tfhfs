/*
 * Author: Markus Stenberg <fingon@iki.fi>
 *
 * Copyright (c) 2018 Markus Stenberg
 *
 * Created:       Fri Feb 16 10:17:18 2018 mstenber
 * Last modified: Wed Feb 21 17:38:18 2018 mstenber
 * Edit time:     9 min
 *
 */

package tree

import "github.com/fingon/go-tfhfs/storage"

type LocationEntry struct {
	Offset, Size uint64
}

type LocationSlice []LocationEntry

type BlockData struct {
	storage.BlockMetadata
	Location LocationSlice
}

type OpEntry struct {
	Location LocationEntry
	Free     bool // free / alloc
}

type OpSlice []OpEntry

type Superblock struct {
	Generation   uint64
	BytesUsed    uint64
	BytesTotal   uint64
	RootLocation LocationSlice
	Pending      OpSlice
}
