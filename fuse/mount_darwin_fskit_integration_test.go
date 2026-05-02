// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && fskit_integration

package fuse_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestFSKitIntegration performs a real FSKit mount on a loopback filesystem
// and exercises stat/read/write/unmount. It requires:
//   - macOS 15.4 or newer
//   - macFUSE 5.3.1+ installed
//   - macFUSE FSKit extension registered and approved (open the macFUSE app
//     once; approve the system extension in System Settings if prompted)
//
// Run with:
//
//	go test -tags='fskit_integration' -run TestFSKitIntegration -v ./fuse/...
func TestFSKitIntegration(t *testing.T) {
	src := t.TempDir()
	mnt := t.TempDir()

	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	root, err := fs.NewLoopbackRoot(src)
	if err != nil {
		t.Fatalf("NewLoopbackRoot: %v", err)
	}

	server, err := fs.Mount(mnt, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Backend: "fskit",
			FsName:  "fskit-integration",
			Name:    "fskit-test",
		},
	})
	if err != nil {
		// Treat clearly environmental MFMount failures as skips, not test failures.
		// These rcs come from mapMFMountErr and indicate the framework wasn't usable.
		msg := err.Error()
		envSkip := strings.Contains(msg, "not registered") ||
			strings.Contains(msg, "not approved") ||
			strings.Contains(msg, "helper tools not installed") ||
			strings.Contains(msg, "FSKit not available")
		if envSkip {
			t.Skipf("FSKit not usable in this environment: %v", err)
		}
		t.Fatalf("fs.Mount: %v", err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Errorf("Unmount: %v", err)
		}
	}()

	// Read existing file through the mount.
	data, err := os.ReadFile(filepath.Join(mnt, "hello.txt"))
	if err != nil {
		t.Fatalf("read through mount: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("read = %q, want %q", data, "hello")
	}

	// Stat through the mount.
	fi, err := os.Stat(filepath.Join(mnt, "hello.txt"))
	if err != nil {
		t.Fatalf("stat through mount: %v", err)
	}
	if fi.Size() != int64(len("hello")) {
		t.Errorf("size = %d, want %d", fi.Size(), len("hello"))
	}

	// Write through the mount; verify the backing dir sees it.
	wantBody := "world\n"
	if err := os.WriteFile(filepath.Join(mnt, "world.txt"), []byte(wantBody), 0o644); err != nil {
		t.Fatalf("write through mount: %v", err)
	}
	// Loopback may need a sync tick.
	time.Sleep(50 * time.Millisecond)
	got, err := os.ReadFile(filepath.Join(src, "world.txt"))
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("backing file = %q, want %q", got, wantBody)
	}
}
