// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"bytes"
	"encoding/binary"
	"sync"
	"unsafe"
)

// entryOutSize is the on-the-wire size of fuse_entry_out. It is 144 bytes
// on darwin (where Attr is 104 bytes due to Crtime/Crtimensec/Flags_) and
// 128 bytes elsewhere; using unsafe.Sizeof picks the correct value at
// compile time on each platform. Used by transformReadDirPlusReply to skip
// past each EntryOut prefix when converting a READDIRPLUS reply payload to
// plain READDIR shape.
var entryOutSize = int(unsafe.Sizeof(EntryOut{}))

// fskitAdapter handles the FSKit-specific FUSE wire quirks that the bridge
// has no hook for. ID translation is NOT done here: when MountOptions.Backend
// is "fskit", the bridge allocates nodeId == StableAttr.Ino directly, so
// FSKit's "address each file by its Ino" wire convention coincides with the
// bridge's identity. The bridge also handles the negative-LOOKUP suppression.
// See go-fuse/fs/bridge.go (newInodeUnlocked, Lookup) and
// go-fuse/fuse/mount_darwin_fskit.md for the design rationale and trade-offs.
//
// The surviving wire quirks are:
//
//  1. READDIR → READDIRPLUS opcode promotion. FSKit issues GETATTR on dirent
//     Inos without a preceding LOOKUP, so the bridge needs to register each
//     child during the READDIR reply. Promoting the opcode forces the bridge
//     to run Lookup() per child; we transform the reply back to plain READDIR
//     shape because that is what FSKit asked for.
//
//  2. com.apple.* SETXATTR error masking. FSKit's CREATE flow stamps system
//     xattrs (com.apple.provenance, etc.) on new inodes and treats SETXATTR
//     failure as fatal, aborting the create. Filesystems that don't accept
//     arbitrary xattrs would otherwise be silently rendered read-only under
//     FSKit, so we rewrite these specific failures to OK.
//
// FSKit's "address volume root by nodeid==0" convention is handled in the
// bridge: under Backend=="fskit" the bridge mints root.nodeId==0 and aliases
// kernelNodeIds[FUSE_ROOT_ID] to the same root, so both inbound (when FSKit
// sends 0) and outbound (EntryNotify on the root must write 0) directions
// use the id FSKit expects without a wire-level rewrite here.
//
// All quirks are properties of macFUSE's FSKit shim, not of FSKit or FUSE.
// A more complete shim would let this file delete entirely.
type fskitAdapter struct {
	mu sync.Mutex
	// promotedReadDir tracks unique IDs of READDIR requests that the
	// adapter promoted to READDIRPLUS so the reply can be converted back
	// to plain READDIR format.
	promotedReadDir map[uint64]bool
	// appleSetxattr tracks unique IDs of SETXATTR requests whose attribute
	// name starts with "com.apple.". See the docstring above.
	appleSetxattr map[uint64]bool
}

func newFSKitAdapter() *fskitAdapter {
	return &fskitAdapter{
		promotedReadDir: make(map[uint64]bool),
		appleSetxattr:   make(map[uint64]bool),
	}
}

// handleInbound rewrites incoming FUSE messages in place: promotes
// READDIR → READDIRPLUS and tracks com.apple.* SETXATTR requests so their
// replies can be masked. nodeid==0 (FSKit's volume-root sentinel) needs no
// rewrite here: the bridge registers the root under both 0 and FUSE_ROOT_ID
// in FSKit mode, so it resolves natively.
func (a *fskitAdapter) handleInbound(msg []byte) {
	if len(msg) < 24 {
		return
	}
	opcode := binary.LittleEndian.Uint32(msg[4:8])
	if opcode == _OP_INIT {
		return
	}
	// SETXATTR body layout on darwin: fuse_in_header(40) + size(4) + flags(4) +
	// position(4) + padding(4) = 56, then null-terminated name. Detect Apple
	// system xattrs that FSKit's create path requires to succeed.
	if opcode == _OP_SETXATTR {
		const nameOff = 56
		if len(msg) > nameOff && bytes.HasPrefix(msg[nameOff:], appleXattrPrefix) {
			unique := binary.LittleEndian.Uint64(msg[8:16])
			a.mu.Lock()
			a.appleSetxattr[unique] = true
			a.mu.Unlock()
		}
		return
	}
}

// wasPromotedReadDir reports whether the request with the given unique was
// originally READDIR and got promoted to READDIRPLUS by handleInbound. The
// entry is cleared on first inspection so the map does not grow unbounded.
func (a *fskitAdapter) wasPromotedReadDir(unique uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.promotedReadDir[unique] {
		delete(a.promotedReadDir, unique)
		return true
	}
	return false
}

// transformReadDirPlusReply converts a READDIRPLUS reply payload (a stream
// of fuse_direntplus entries: fuse_entry_out + fuse_dirent) into a plain
// READDIR reply payload (just the fuse_dirent portions). Returns the
// transformed payload.
//
// fuse_dirent layout: ino(8) off(8) namelen(4) type(4) name (padded to 8).
func (a *fskitAdapter) transformReadDirPlusReply(payload []byte) []byte {
	const direntFixed = 24
	out := make([]byte, 0, len(payload))
	cursor := 0
	for cursor < len(payload) {
		if cursor+entryOutSize > len(payload) {
			// Truncated trailing entry; drop it.
			break
		}
		direntStart := cursor + entryOutSize
		if direntStart+direntFixed > len(payload) {
			break
		}
		namelen := binary.LittleEndian.Uint32(payload[direntStart+16 : direntStart+20])
		paddedName := (int(namelen) + 7) &^ 7
		direntEnd := direntStart + direntFixed + paddedName
		if direntEnd > len(payload) {
			break
		}
		out = append(out, payload[direntStart:direntEnd]...)
		cursor = direntEnd
	}
	return out
}

// shouldMaskSetxattrError reports whether a SETXATTR reply with this unique
// ID was for a com.apple.* attribute and should have its error rewritten to
// OK. The mapping entry is cleared on first inspection.
func (a *fskitAdapter) shouldMaskSetxattrError(unique uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appleSetxattr[unique] {
		delete(a.appleSetxattr, unique)
		return true
	}
	return false
}

// appleXattrPrefix is the prefix shared by Apple system xattrs that FSKit's
// create flow expects to succeed.
var appleXattrPrefix = []byte("com.apple.")
