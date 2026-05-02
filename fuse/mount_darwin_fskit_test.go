// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

package fuse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMapMFMountErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rc      int32
		wantNil bool
		wantSub string
	}{
		{0, true, ""},
		{1, false, "helper tools not installed"},
		{2, false, "FSKit extension not registered"},
		{3, false, "FSKit extension is not approved"},
		{4, false, "FSKit mount failed (rc=4)"},
		{5, false, "FSKit mount failed (rc=5)"},
		{6, false, "FSKit mount failed (rc=6)"},
		{7, false, "FSKit mount failed (rc=7)"},
		{-1, false, "FSKit mount failed (rc=-1)"},
		{99, false, "FSKit mount failed (rc=99)"},
	}
	for _, tt := range tests {
		err := mapMFMountErr(tt.rc)
		if tt.wantNil {
			if err != nil {
				t.Errorf("rc=%d: got %v, want nil", tt.rc, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("rc=%d: got nil, want error containing %q", tt.rc, tt.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantSub) {
			t.Errorf("rc=%d: got %q, want substring %q", tt.rc, err.Error(), tt.wantSub)
		}
	}
}

// captureCall records the args MFMount was called with.
type captureCall struct {
	mountPoint string
	options    string
	quiet      bool
	socket     int32
	calls      atomic.Int32
}

func newFakeLoader(rc int32, capArgs *captureCall) mfMountLoader {
	return func() (mfMountFunc, error) {
		fn := mfMountFunc(func(mp, o string, q bool, s int32) int32 {
			if capArgs != nil {
				capArgs.mountPoint = mp
				capArgs.options = o
				capArgs.quiet = q
				capArgs.socket = s
				capArgs.calls.Add(1)
			}
			return rc
		})
		return fn, nil
	}
}

func TestMountFSKit_LoaderFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("synthetic load failure")
	loader := func() (mfMountFunc, error) { return nil, want }
	ready := make(chan error, 1)

	fd, err := mountFSKit("/mnt/x", &MountOptions{Backend: "fskit"}, ready, loader)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("got err=%v, want wraps %v", err, want)
	}
	if fd >= 0 {
		t.Errorf("got fd=%d, want -1", fd)
	}
	select {
	case e := <-ready:
		t.Errorf("ready unexpectedly received %v", e)
	default:
		// good — ready not signaled when load itself fails
	}
}

func TestMountFSKit_Success(t *testing.T) {
	t.Parallel()
	capArgs := &captureCall{}
	loader := newFakeLoader(0, capArgs)
	ready := make(chan error, 1)

	opts := &MountOptions{
		Backend: "fskit",
		Options: []string{"allow_other"},
		FsName:  "fskit-test",
	}

	fd, err := mountFSKit("/mnt/test", opts, ready, loader)
	if err != nil {
		t.Fatalf("mountFSKit: %v", err)
	}
	if fd < 0 {
		t.Fatalf("got fd=%d, want >= 0", fd)
	}
	defer syscall.Close(fd)

	select {
	case e := <-ready:
		if e != nil {
			t.Errorf("ready got %v, want nil", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ready")
	}

	if capArgs.calls.Load() != 1 {
		t.Errorf("MFMount called %d times, want 1", capArgs.calls.Load())
	}
	if capArgs.mountPoint != "/mnt/test" {
		t.Errorf("mountPoint = %q, want /mnt/test", capArgs.mountPoint)
	}
	if !capArgs.quiet {
		t.Error("quiet = false, want true")
	}
	if !strings.Contains(capArgs.options, "allow_other") {
		t.Errorf("options %q missing allow_other", capArgs.options)
	}
}

func TestMountFSKit_RcMappedToReady(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rc      int32
		wantSub string
	}{
		{2, "FSKit extension not registered"},
		{3, "FSKit extension is not approved"},
		{4, "FSKit mount failed (rc=4)"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(fmt.Sprintf("rc=%d", tt.rc), func(t *testing.T) {
			t.Parallel()
			loader := newFakeLoader(tt.rc, nil)
			ready := make(chan error, 1)
			fd, err := mountFSKit("/mnt/x", &MountOptions{Backend: "fskit"}, ready, loader)
			if err != nil {
				t.Fatalf("mountFSKit returned err=%v; expected non-zero rc to flow through ready, not direct return", err)
			}
			defer syscall.Close(fd)
			select {
			case e := <-ready:
				if e == nil {
					t.Fatalf("ready got nil, want error containing %q", tt.wantSub)
				}
				if !strings.Contains(e.Error(), tt.wantSub) {
					t.Errorf("ready got %q, want substring %q", e.Error(), tt.wantSub)
				}
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for ready")
			}
		})
	}
}

func TestMountFSKit_FdIsSocketStream(t *testing.T) {
	t.Parallel()
	loader := newFakeLoader(0, nil)
	ready := make(chan error, 1)
	fd, err := mountFSKit("/mnt/x", &MountOptions{Backend: "fskit"}, ready, loader)
	if err != nil {
		t.Fatalf("mountFSKit: %v", err)
	}
	defer syscall.Close(fd)
	<-ready

	sotype, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		t.Fatalf("getsockopt SO_TYPE: %v", err)
	}
	if sotype != syscall.SOCK_STREAM {
		t.Errorf("SO_TYPE = %d, want SOCK_STREAM (%d)", sotype, syscall.SOCK_STREAM)
	}
}

func TestNewCachingLoader_OnlyCallsInnerOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	inner := func() (mfMountFunc, error) {
		calls.Add(1)
		return mfMountFunc(func(string, string, bool, int32) int32 { return 0 }), nil
	}
	cached := newCachingLoader(inner)

	for i := 0; i < 5; i++ {
		if _, err := cached(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("inner called %d times, want 1", got)
	}
}

func TestNewCachingLoader_CachesError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("dlopen failed")
	var calls atomic.Int32
	inner := func() (mfMountFunc, error) {
		calls.Add(1)
		return nil, wantErr
	}
	cached := newCachingLoader(inner)

	for i := 0; i < 3; i++ {
		_, err := cached()
		if !errors.Is(err, wantErr) {
			t.Fatalf("call %d: got %v, want wraps %v", i, err, wantErr)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("inner called %d times, want 1", got)
	}
}

func TestFSKitAdapter_HandleInboundLeavesNodeIdZeroAlone(t *testing.T) {
	// Under FSKit, the bridge registers the root at both nodeId==0 and
	// nodeId==FUSE_ROOT_ID, so the adapter no longer needs to rewrite
	// nodeid==0 → FUSE_ROOT_ID at the wire. This test guards against a
	// regression that re-introduces the rewrite (which would mask any
	// future nodeid==0 routing bugs in the bridge).
	a := newFSKitAdapter()
	msg := make([]byte, 40)
	binary.LittleEndian.PutUint32(msg[0:4], 40)
	binary.LittleEndian.PutUint32(msg[4:8], _OP_GETATTR)
	binary.LittleEndian.PutUint64(msg[8:16], 1)
	binary.LittleEndian.PutUint64(msg[16:24], 0)

	a.handleInbound(msg)

	if got := binary.LittleEndian.Uint64(msg[16:24]); got != 0 {
		t.Fatalf("NodeId after handleInbound = %d, want 0 (rewrite must not happen)", got)
	}
}

func TestFSKitAdapter_HandleInboundLeavesInitNodeIdAlone(t *testing.T) {
	a := newFSKitAdapter()
	msg := make([]byte, 40)
	binary.LittleEndian.PutUint32(msg[4:8], _OP_INIT)
	// NodeId already 0; ensure handleInbound's INIT early-return runs
	// before the rewrite so init traffic isn't perturbed.
	a.handleInbound(msg)
	if got := binary.LittleEndian.Uint64(msg[16:24]); got != 0 {
		t.Fatalf("INIT NodeId = %d, want 0 (rewrite must not run on INIT)", got)
	}
}

func TestFSKitAdapter_HandleInboundLeavesNonZeroNodeIdAlone(t *testing.T) {
	a := newFSKitAdapter()
	msg := make([]byte, 40)
	binary.LittleEndian.PutUint32(msg[4:8], _OP_GETATTR)
	binary.LittleEndian.PutUint64(msg[16:24], 42)
	a.handleInbound(msg)
	if got := binary.LittleEndian.Uint64(msg[16:24]); got != 42 {
		t.Fatalf("NodeId = %d, want 42 (non-zero ids must pass through)", got)
	}
}

// buildReadDirPlusPayload synthesizes a READDIRPLUS reply payload (a stream of
// fuse_direntplus = fuse_entry_out + fuse_dirent + name + padding) the way the
// bridge would produce it. Names are utf-8 bytes; ino is the dirent ino.
func buildReadDirPlusPayload(t *testing.T, entries []struct {
	name string
	ino  uint64
}) []byte {
	t.Helper()
	const direntFixed = 24
	out := []byte{}
	for i, e := range entries {
		// Allocate one fuse_direntplus = entryOutSize + 24 + len(name) + pad.
		paddedName := (len(e.name) + 7) &^ 7
		size := entryOutSize + direntFixed + paddedName
		buf := make([]byte, size)
		// Fill EntryOut.NodeId so we can confirm transform strips it.
		binary.LittleEndian.PutUint64(buf[0:8], e.ino) // EntryOut.NodeId
		// dirent_fixed at offset entryOutSize: ino, off, namelen, type
		direntStart := entryOutSize
		binary.LittleEndian.PutUint64(buf[direntStart:direntStart+8], e.ino)
		binary.LittleEndian.PutUint64(buf[direntStart+8:direntStart+16], uint64(i+1))
		binary.LittleEndian.PutUint32(buf[direntStart+16:direntStart+20], uint32(len(e.name)))
		binary.LittleEndian.PutUint32(buf[direntStart+20:direntStart+24], 8) // type S_IFREG>>12
		copy(buf[direntStart+direntFixed:], e.name)
		out = append(out, buf...)
	}
	return out
}

func TestFSKitAdapter_TransformReadDirPlusReply_NoDuplicates(t *testing.T) {
	entries := []struct {
		name string
		ino  uint64
	}{
		{".dockerenv", 100},
		{"bin", 101},
		{"etc", 102},
		{"var", 103},
	}
	payload := buildReadDirPlusPayload(t, entries)

	a := newFSKitAdapter()
	out := a.transformReadDirPlusReply(payload)

	// Expected output: exactly len(entries) fuse_dirent entries
	// (24 bytes header + padded name).
	const direntFixed = 24
	cursor := 0
	for i, e := range entries {
		if cursor+direntFixed > len(out) {
			t.Fatalf("entry %d: truncated; cursor=%d len(out)=%d", i, cursor, len(out))
		}
		gotIno := binary.LittleEndian.Uint64(out[cursor : cursor+8])
		gotOff := binary.LittleEndian.Uint64(out[cursor+8 : cursor+16])
		gotNamelen := binary.LittleEndian.Uint32(out[cursor+16 : cursor+20])
		if gotIno != e.ino {
			t.Errorf("entry %d: ino=%d, want %d", i, gotIno, e.ino)
		}
		if gotOff != uint64(i+1) {
			t.Errorf("entry %d: off=%d, want %d", i, gotOff, i+1)
		}
		if int(gotNamelen) != len(e.name) {
			t.Errorf("entry %d: namelen=%d, want %d", i, gotNamelen, len(e.name))
		}
		gotName := string(out[cursor+direntFixed : cursor+direntFixed+int(gotNamelen)])
		if gotName != e.name {
			t.Errorf("entry %d: name=%q, want %q", i, gotName, e.name)
		}
		paddedName := (len(e.name) + 7) &^ 7
		cursor += direntFixed + paddedName
	}
	if cursor != len(out) {
		t.Errorf("trailing %d unparsed bytes after %d entries (out=%d)", len(out)-cursor, len(entries), len(out))
	}

}

func TestFSKitAdapter_TransformReadDirPlusReply_RealDirectoryFromBugReport(t *testing.T) {
	// Reproduce the exact directory listing from the doubling bug report:
	// 15 entries that ls/os.listdir surface twice each on the FSKit mount.
	// Confirms (or rules out) that transformReadDirPlusReply is responsible.
	entries := []struct {
		name string
		ino  uint64
	}{
		{".dockerenv", 100},
		{"bin", 101},
		{"etc", 102},
		{"home", 103},
		{"lib", 104},
		{"media", 105},
		{"mnt", 106},
		{"opt", 107},
		{"root", 108},
		{"run", 109},
		{"sbin", 110},
		{"srv", 111},
		{"tmp", 112},
		{"usr", 113},
		{"var", 114},
	}
	payload := buildReadDirPlusPayload(t, entries)

	a := newFSKitAdapter()
	out := a.transformReadDirPlusReply(payload)

	// Walk the resulting dirent stream and count each parsed name.
	const direntFixed = 24
	got := []string{}
	cursor := 0
	for cursor+direntFixed <= len(out) {
		namelen := int(binary.LittleEndian.Uint32(out[cursor+16 : cursor+20]))
		if cursor+direntFixed+namelen > len(out) {
			break
		}
		name := string(out[cursor+direntFixed : cursor+direntFixed+namelen])
		got = append(got, name)
		paddedName := (namelen + 7) &^ 7
		cursor += direntFixed + paddedName
	}
	if len(got) != len(entries) {
		t.Errorf("transform produced %d dirents, want %d: %v", len(got), len(entries), got)
	}
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
	}
	for n, c := range seen {
		if c > 1 {
			t.Errorf("dirent %q appears %d times in transform output", n, c)
		}
	}
}

