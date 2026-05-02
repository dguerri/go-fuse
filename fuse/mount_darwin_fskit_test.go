// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

package fuse

import (
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
