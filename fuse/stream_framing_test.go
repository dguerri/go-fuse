// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"encoding/binary"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"
)

// fuseMsg builds a FUSE wire message: 40-byte header where header.len is set
// to 40+len(body), followed by body.
func fuseMsg(opcode uint32, unique uint64, body []byte) []byte {
	msg := make([]byte, fuseInHeaderSize+len(body))
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	binary.LittleEndian.PutUint32(msg[4:8], opcode)
	binary.LittleEndian.PutUint64(msg[8:16], unique)
	copy(msg[fuseInHeaderSize:], body)
	return msg
}

// streamPair returns a connected SOCK_STREAM pair (reader, writer).
func streamPair(t *testing.T) (rfd, wfd int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
	})
	return fds[0], fds[1]
}

func writeAll(t *testing.T, fd int, b []byte) {
	t.Helper()
	for off := 0; off < len(b); {
		n, err := syscall.Write(fd, b[off:])
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		off += n
	}
}

// TestReadFramedFUSEMessage_SingleMessage verifies the simple case: one
// complete message arrives in one piece.
func TestReadFramedFUSEMessage_SingleMessage(t *testing.T) {
	rfd, wfd := streamPair(t)
	body := []byte("hello world!")
	msg := fuseMsg(26 /*INIT*/, 1, body)
	writeAll(t, wfd, msg)

	dest := make([]byte, 1024)
	n, err := readFramedFUSEMessage(rfd, dest)
	if err != nil {
		t.Fatalf("readFramedFUSEMessage: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("n=%d want %d", n, len(msg))
	}
	if string(dest[fuseInHeaderSize:n]) != string(body) {
		t.Fatalf("body mismatch: got %q want %q", dest[fuseInHeaderSize:n], body)
	}
}

// TestReadFramedFUSEMessage_TwoMessages verifies that when two messages are
// pre-queued in the kernel buffer (so a single recv could return both), the
// reader still returns exactly one per call.
func TestReadFramedFUSEMessage_TwoMessages(t *testing.T) {
	rfd, wfd := streamPair(t)
	m1 := fuseMsg(26 /*INIT*/, 1, []byte("first body bytes"))
	m2 := fuseMsg(17 /*STATFS*/, 2, nil)
	writeAll(t, wfd, append(append([]byte{}, m1...), m2...))

	dest := make([]byte, 1024)
	n1, err := readFramedFUSEMessage(rfd, dest)
	if err != nil {
		t.Fatalf("read m1: %v", err)
	}
	if n1 != len(m1) {
		t.Fatalf("m1 n=%d want %d (reader gobbled past message boundary)", n1, len(m1))
	}
	if op := binary.LittleEndian.Uint32(dest[4:8]); op != 26 {
		t.Fatalf("m1 opcode=%d want 26", op)
	}

	n2, err := readFramedFUSEMessage(rfd, dest)
	if err != nil {
		t.Fatalf("read m2: %v", err)
	}
	if n2 != len(m2) {
		t.Fatalf("m2 n=%d want %d", n2, len(m2))
	}
	if op := binary.LittleEndian.Uint32(dest[4:8]); op != 17 {
		t.Fatalf("m2 opcode=%d want 17", op)
	}
	if uniq := binary.LittleEndian.Uint64(dest[8:16]); uniq != 2 {
		t.Fatalf("m2 unique=%d want 2", uniq)
	}
}

// TestReadFramedFUSEMessage_SplitWrites verifies that a message delivered in
// fragments (header chunk + body chunk + body chunk) is reassembled.
func TestReadFramedFUSEMessage_SplitWrites(t *testing.T) {
	rfd, wfd := streamPair(t)
	body := make([]byte, 200)
	for i := range body {
		body[i] = byte(i)
	}
	msg := fuseMsg(3 /*GETATTR*/, 7, body)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Write header in two chunks, then body in three chunks, with
		// small sleeps so the reader is forced to loop.
		writeAll(t, wfd, msg[:10])
		time.Sleep(5 * time.Millisecond)
		writeAll(t, wfd, msg[10:fuseInHeaderSize])
		time.Sleep(5 * time.Millisecond)
		writeAll(t, wfd, msg[fuseInHeaderSize:fuseInHeaderSize+50])
		time.Sleep(5 * time.Millisecond)
		writeAll(t, wfd, msg[fuseInHeaderSize+50:])
	}()

	dest := make([]byte, 1024)
	n, err := readFramedFUSEMessage(rfd, dest)
	if err != nil {
		t.Fatalf("readFramedFUSEMessage: %v", err)
	}
	<-done
	if n != len(msg) {
		t.Fatalf("n=%d want %d", n, len(msg))
	}
	for i, b := range dest[fuseInHeaderSize:n] {
		if b != byte(i) {
			t.Fatalf("body[%d]=%d want %d", i, b, byte(i))
		}
	}
}

// TestReadFramedFUSEMessage_HeaderOnly verifies a 40-byte (body-less) message
// is handled without trying to read further.
func TestReadFramedFUSEMessage_HeaderOnly(t *testing.T) {
	rfd, wfd := streamPair(t)
	msg := fuseMsg(17 /*STATFS*/, 5, nil)
	if len(msg) != fuseInHeaderSize {
		t.Fatalf("setup: msg len %d want %d", len(msg), fuseInHeaderSize)
	}
	writeAll(t, wfd, msg)

	dest := make([]byte, 1024)
	n, err := readFramedFUSEMessage(rfd, dest)
	if err != nil {
		t.Fatalf("readFramedFUSEMessage: %v", err)
	}
	if n != fuseInHeaderSize {
		t.Fatalf("n=%d want %d", n, fuseInHeaderSize)
	}
}

// TestReadFramedFUSEMessage_PeerCloseClean returns io.EOF when the peer
// closes before sending anything.
func TestReadFramedFUSEMessage_PeerCloseClean(t *testing.T) {
	rfd, wfd := streamPair(t)
	syscall.Close(wfd)

	dest := make([]byte, 1024)
	_, err := readFramedFUSEMessage(rfd, dest)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v want io.EOF", err)
	}
}

// TestReadFramedFUSEMessage_PeerCloseMidMessage returns io.ErrUnexpectedEOF
// when the peer closes after a partial header.
func TestReadFramedFUSEMessage_PeerCloseMidMessage(t *testing.T) {
	rfd, wfd := streamPair(t)
	// Write a partial header then close.
	writeAll(t, wfd, []byte{1, 2, 3, 4, 5})
	syscall.Close(wfd)

	dest := make([]byte, 1024)
	_, err := readFramedFUSEMessage(rfd, dest)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v want io.ErrUnexpectedEOF", err)
	}
}

// TestReadFramedFUSEMessage_BogusHeaderLen returns an error when header.len
// is below the minimum.
func TestReadFramedFUSEMessage_BogusHeaderLen(t *testing.T) {
	rfd, wfd := streamPair(t)
	bad := make([]byte, fuseInHeaderSize)
	binary.LittleEndian.PutUint32(bad[0:4], 8) // bogus
	writeAll(t, wfd, bad)

	dest := make([]byte, 1024)
	_, err := readFramedFUSEMessage(rfd, dest)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestReadFramedFUSEMessage_ShortDest returns io.ErrShortBuffer when dest is
// too small for the header alone.
func TestReadFramedFUSEMessage_ShortDest(t *testing.T) {
	rfd, _ := streamPair(t)
	dest := make([]byte, 10)
	_, err := readFramedFUSEMessage(rfd, dest)
	if !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("err=%v want io.ErrShortBuffer", err)
	}
}

// TestReadFramedFUSEMessage_DestTooSmallForBody returns io.ErrShortBuffer
// when dest can hold the header but not the announced body.
func TestReadFramedFUSEMessage_DestTooSmallForBody(t *testing.T) {
	rfd, wfd := streamPair(t)
	hdr := make([]byte, fuseInHeaderSize)
	binary.LittleEndian.PutUint32(hdr[0:4], 1024) // claim 1024-byte msg
	writeAll(t, wfd, hdr)

	dest := make([]byte, 64)
	_, err := readFramedFUSEMessage(rfd, dest)
	if !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("err=%v want io.ErrShortBuffer", err)
	}
}
