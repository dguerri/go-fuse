// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

package fuse

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

// mapMFMountErr translates an MFMount return code into a Go error with an
// actionable message. Codes are sourced from macFUSE's Mount/Mount/Mounter.swift
// (MountError.init(error:mountCommandStatus:)).
func mapMFMountErr(rc int32) error {
	switch rc {
	case 0:
		return nil
	case 1:
		return errors.New("FSKit mount failed: macFUSE helper tools not installed; open the macFUSE app once to install them")
	case 2:
		return errors.New("FSKit mount failed: macFUSE FSKit extension not registered; open /Library/Filesystems/macfuse.fs/Contents/Resources/macfuse.app once to register it")
	case 3:
		return errors.New("FSKit mount failed: macFUSE FSKit extension is not approved; enable it in System Settings → General → Login Items & Extensions → File System Extensions")
	default:
		return fmt.Errorf("FSKit mount failed (rc=%d)", rc)
	}
}

// mfMountFunc is the Go signature of the C function MFMount exported from
// /Library/Filesystems/macfuse.fs/Contents/Frameworks/MFMount.framework.
//
// C signature:
//   int32_t MFMount(const char *mountpoint, const char *options, bool quiet, int32_t socket);
//
// purego converts Go strings to null-terminated C strings automatically.
type mfMountFunc func(mountpoint, options string, quiet bool, socket int32) int32

// mfMountLoader resolves an mfMountFunc, typically by dlopening MFMount.framework
// and binding the MFMount symbol. Tests pass a fake loader that returns a
// closure with predetermined behaviour.
type mfMountLoader func() (mfMountFunc, error)

// mountFSKit performs an FSKit-backed mount. It calls load() to obtain an
// MFMount function pointer (production code passes a caching loader that
// dlopens MFMount.framework once per process; tests pass a fake), creates a
// socketpair, and spawns a goroutine that invokes MFMount on the remote half.
// The returned fd is the local half of the socketpair, suitable for use as
// the FUSE channel. The ready channel is signaled with the result of the
// MFMount call (nil on success, mapped error otherwise) and then closed.
//
// On loader failure, mountFSKit returns the error directly without spawning
// a goroutine and without signaling ready.
//
// The ready channel must be buffered (capacity >= 1) or have an active
// reader; mountFSKit's goroutine sends on ready exactly once and would
// otherwise block forever, leaking the goroutine and its socket end.
func mountFSKit(mountPoint string, opts *MountOptions, ready chan<- error, load mfMountLoader) (int, error) {
	fn, err := load()
	if err != nil {
		return -1, err
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("FSKit mount: socketpair: %w", err)
	}

	optsStr := strings.Join(opts.optionsStrings(), ",")

	// CLOEXEC on our local fd so it doesn't leak to children.
	syscall.CloseOnExec(fds[0])

	go func() {
		rc := fn(mountPoint, optsStr, true, int32(fds[1]))
		// MFMount has finished using its end of the socket; close it.
		syscall.Close(fds[1])
		ready <- mapMFMountErr(rc)
		close(ready)
	}()

	return fds[0], nil
}

// mfmountFrameworkPath is the canonical path to the macFUSE MFMount framework
// binary. macFUSE 5.3.1+ installs this; earlier versions and Linux/FreeBSD do not.
const mfmountFrameworkPath = "/Library/Filesystems/macfuse.fs/Contents/Frameworks/MFMount.framework/MFMount"

// realLoadMFMount dlopens MFMount.framework via purego and binds the MFMount
// symbol. Returns a clear error if the framework is not installed or the
// symbol is missing.
func realLoadMFMount() (fn mfMountFunc, err error) {
	h, err := purego.Dlopen(mfmountFrameworkPath, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("FSKit not available: cannot load %s: %w", mfmountFrameworkPath, err)
	}
	// purego.RegisterLibFunc panics (does not return error) when the symbol
	// is missing; recover converts it to a typed error. Verified against
	// github.com/ebitengine/purego v0.10.0.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("FSKit not available: MFMount symbol missing in %s: %v", mfmountFrameworkPath, r)
		}
	}()
	purego.RegisterLibFunc(&fn, h, "MFMount")
	return fn, nil
}

// prodLoader is the production loader used by mount_darwin.go's switch. It
// dlopens MFMount.framework at most once per process and caches the result
// (success or failure). Tests pass their own fake loader instead.
var (
	mfMountOnce sync.Once
	mfMountFn   mfMountFunc
	mfMountErr  error
)

func prodLoader() (mfMountFunc, error) {
	mfMountOnce.Do(func() { mfMountFn, mfMountErr = realLoadMFMount() })
	return mfMountFn, mfMountErr
}
