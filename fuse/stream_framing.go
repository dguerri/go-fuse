// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"encoding/binary"
	"fmt"
	"io"
	"syscall"
)

// fuseInHeaderSize is the size of fuse_in_header on the wire (40 bytes:
// len(4) + opcode(4) + unique(8) + nodeid(8) + uid(4) + gid(4) + pid(4) +
// padding(4)). It is fixed across all FUSE protocol versions go-fuse cares
// about, so we hard-code it rather than relying on unsafe.Sizeof of the Go
// struct (which can include extra padding on some architectures).
const fuseInHeaderSize = 40

// readFramedFUSEMessage reads exactly one FUSE request from a SOCK_STREAM
// connection into dest, using header-first framing: read the 40-byte
// fuse_in_header, parse header.len, then read the remaining body bytes.
//
// This is required for the macOS FSKit backend, where the userspace channel
// is an AF_UNIX SOCK_STREAM socket rather than the /dev/fuse character
// device. On /dev/fuse each read() returns exactly one request; on a stream
// socket a read may return a partial header, a single message, or several
// concatenated messages, so the caller must reassemble by length.
//
// Returns the total number of bytes written into dest (== header.len on
// success). EINTR is retried transparently; other errors are surfaced.
// Returns io.ErrShortBuffer if dest cannot hold the message.
func readFramedFUSEMessage(fd int, dest []byte) (int, error) {
	if len(dest) < fuseInHeaderSize {
		return 0, io.ErrShortBuffer
	}
	if err := readFull(fd, dest[:fuseInHeaderSize]); err != nil {
		return 0, err
	}
	msgLen := int(binary.LittleEndian.Uint32(dest[0:4]))
	if msgLen < fuseInHeaderSize {
		return 0, fmt.Errorf("fuse stream: bogus header.len=%d (<%d)", msgLen, fuseInHeaderSize)
	}
	if msgLen > len(dest) {
		return 0, io.ErrShortBuffer
	}
	if msgLen == fuseInHeaderSize {
		return fuseInHeaderSize, nil
	}
	if err := readFull(fd, dest[fuseInHeaderSize:msgLen]); err != nil {
		return 0, err
	}
	return msgLen, nil
}

// readFull reads exactly len(buf) bytes from fd. Returns io.EOF if the peer
// closes before any byte is read, io.ErrUnexpectedEOF if it closes
// mid-buffer. EINTR is retried.
func readFull(fd int, buf []byte) error {
	got := 0
	for got < len(buf) {
		n, err := syscall.Read(fd, buf[got:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			if got == 0 {
				return io.EOF
			}
			return io.ErrUnexpectedEOF
		}
		got += n
	}
	return nil
}
