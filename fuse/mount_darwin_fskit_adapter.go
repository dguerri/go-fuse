// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"bytes"
	"encoding/binary"
	"sync"
)

// fskitAdapter handles the one FSKit-specific quirk that survives macFUSE
// 5.3.1: during file creation the FSKit coordinator stamps Apple system
// xattrs (notably com.apple.provenance) on the new inode and treats a
// SETXATTR failure as fatal, aborting the create without ever issuing
// FUSE_CREATE. Filesystems that don't accept arbitrary xattrs would otherwise
// be silently rendered read-only under FSKit, so the adapter remembers which
// in-flight SETXATTR requests target com.apple.* names and rewrites a failing
// reply to OK so the create proceeds. The legacy kext path never issues these
// probes, so the workaround is scoped to FSKit.
//
// Earlier go-fuse FSKit support (targeting macFUSE 5.2.x) also had to bridge
// between FSKit's wire convention — which addressed each file by its inode
// number rather than the FUSE NodeId go-fuse returned — and standard FUSE
// semantics. macFUSE 5.3.1 changed the FSKit backend to use the node
// identifier and generation as the authoritative handle (issues #1166/#1167),
// so that inode<->NodeId translation is no longer required and has been
// removed. If a future macFUSE release also stops probing com.apple.* xattrs
// during create, this adapter can be dropped entirely.
type fskitAdapter struct {
	mu sync.Mutex
	// appleSetxattr tracks unique IDs of in-flight SETXATTR requests whose
	// attribute name starts with "com.apple." (see the type doc for why).
	appleSetxattr map[uint64]bool
}

func newFSKitAdapter() *fskitAdapter {
	return &fskitAdapter{
		appleSetxattr: make(map[uint64]bool),
	}
}

// handleInbound inspects an incoming FUSE request before it is dispatched and
// records com.apple.* SETXATTR requests so the eventual reply can be masked.
// Under macFUSE 5.3.1 the request's NodeId is already the handle go-fuse
// returned, so no header rewriting is performed here.
func (a *fskitAdapter) handleInbound(msg []byte) {
	if len(msg) < 24 {
		return
	}
	if binary.LittleEndian.Uint32(msg[4:8]) != _OP_SETXATTR {
		return
	}
	// SETXATTR body layout on darwin: fuse_in_header(40) + size(4) + flags(4) +
	// position(4) + padding(4) = 56, then the null-terminated name.
	const nameOff = 56
	if len(msg) > nameOff && bytes.HasPrefix(msg[nameOff:], appleXattrPrefix) {
		unique := binary.LittleEndian.Uint64(msg[8:16])
		a.mu.Lock()
		a.appleSetxattr[unique] = true
		a.mu.Unlock()
	}
}

// shouldMaskSetxattrError reports whether a SETXATTR reply with this unique ID
// was for a com.apple.* attribute. The bookkeeping entry is cleared on
// inspection regardless of the result, so callers must invoke it exactly once
// per SETXATTR reply (whether it succeeded or failed) to avoid leaking
// entries.
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
// create flow expects to succeed. See the fskitAdapter type doc for context.
var appleXattrPrefix = []byte("com.apple.")
