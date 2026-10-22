// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

// Package fskit implements a FUSE file system frontend on macOS using the
// macFUSE 5.3.0+ FSKit user-space framework and fuse.ProtocolServer.
package fskit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// mfmountFrameworkPath is the canonical path to the macFUSE MFMount framework
// binary installed by macFUSE 5.3.0+.
const mfmountFrameworkPath = "/Library/Filesystems/macfuse.fs/Contents/Frameworks/MFMount.framework/MFMount"

// MFMountResult enum values from MFMount.h.
const (
	mfMountResultSuccess                             int32 = 0
	mfMountResultUnsupportedOSVersion                int32 = 1
	mfMountResultHelperToolsInstallationFailed       int32 = 2
	mfMountResultFileSystemExtensionNotFound         int32 = 3
	mfMountResultFileSystemExtensionRequiresApproval int32 = 4
	mfMountResultCreatingMountPointFailed            int32 = 5
	mfMountResultUnexpectedFailure                   int32 = -1
)

func mapMFMountErr(rc int32) error {
	switch rc {
	case mfMountResultSuccess:
		return nil
	case mfMountResultUnsupportedOSVersion:
		return errors.New("fskit: unsupported version of macOS")
	case mfMountResultHelperToolsInstallationFailed:
		return errors.New("fskit: macFUSE helper tools not installed; open the macFUSE app once to install them")
	case mfMountResultFileSystemExtensionNotFound:
		return errors.New("fskit: macFUSE FSKit extension not registered; open /Library/Filesystems/macfuse.fs/Contents/Resources/macfuse.app once to register it")
	case mfMountResultFileSystemExtensionRequiresApproval:
		return errors.New("fskit: macFUSE FSKit extension is not approved; enable it in System Settings → General → Login Items & Extensions → File System Extensions")
	case mfMountResultCreatingMountPointFailed:
		return errors.New("fskit: creating mount point failed")
	case mfMountResultUnexpectedFailure:
		return errors.New("fskit: unexpected failure")
	default:
		return fmt.Errorf("fskit: mount failed (rc=%d)", rc)
	}
}

// darwinIovec matches <sys/uio.h> on 64-bit Darwin.
type darwinIovec struct {
	base unsafe.Pointer
	len  uint64
}

// C function types for MFMount.framework symbols.
type (
	fnMFChannelCreate         func() uintptr
	fnMFMount                 func(channel uintptr, mountPoint, options string, quiet bool) int32
	fnMFMessageGetBodySize    func(message uintptr) int
	fnMFMessageGetBodyBuffers func(message uintptr, buffers *unsafe.Pointer) int
	fnMFChannelInterrupt      func(channel uintptr) bool
	fnMFChannelClose          func(channel uintptr) bool
	fnMFRetain                func(ref uintptr) uintptr
	fnMFRelease               func(ref uintptr)
)

type mfMountAPI struct {
	channelCreate         fnMFChannelCreate
	mount                 fnMFMount
	symCopyNextMessage    uintptr
	messageGetBodySize    fnMFMessageGetBodySize
	messageGetBodyBuffers fnMFMessageGetBodyBuffers
	symSendMessage        uintptr
	channelInterrupt      fnMFChannelInterrupt
	channelClose          fnMFChannelClose
	retain                fnMFRetain
	release               fnMFRelease
}

var (
	mfAPIOnce sync.Once
	mfAPI     *mfMountAPI
	mfAPIErr  error
)

func loadMFMountAPI() (*mfMountAPI, error) {
	mfAPIOnce.Do(func() {
		h, err := purego.Dlopen(mfmountFrameworkPath, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			mfAPIErr = fmt.Errorf("fskit: cannot load %s: %w", mfmountFrameworkPath, err)
			return
		}

		api := &mfMountAPI{}
		defer func() {
			if r := recover(); r != nil {
				mfAPIErr = fmt.Errorf("fskit: missing symbol in %s: %v", mfmountFrameworkPath, r)
				mfAPI = nil
			}
		}()

		symCopy, err := purego.Dlsym(h, "MFChannelCopyNextMessage")
		if err != nil {
			mfAPIErr = fmt.Errorf("fskit: missing MFChannelCopyNextMessage: %w", err)
			return
		}
		api.symCopyNextMessage = symCopy

		symSend, err := purego.Dlsym(h, "MFChannelSendMessage")
		if err != nil {
			mfAPIErr = fmt.Errorf("fskit: missing MFChannelSendMessage: %w", err)
			return
		}
		api.symSendMessage = symSend

		purego.RegisterLibFunc(&api.channelCreate, h, "MFChannelCreate")
		purego.RegisterLibFunc(&api.mount, h, "MFMount")
		purego.RegisterLibFunc(&api.messageGetBodySize, h, "MFMessageGetBodySize")
		purego.RegisterLibFunc(&api.messageGetBodyBuffers, h, "MFMessageGetBodyBuffers")
		purego.RegisterLibFunc(&api.channelInterrupt, h, "MFChannelInterrupt")
		purego.RegisterLibFunc(&api.channelClose, h, "MFChannelClose")
		purego.RegisterLibFunc(&api.retain, h, "MFRetain")
		purego.RegisterLibFunc(&api.release, h, "MFRelease")

		mfAPI = api
	})
	return mfAPI, mfAPIErr
}

// Available reports whether the macFUSE FSKit framework is installed and usable
// on the current system.
func Available() bool {
	_, err := loadMFMountAPI()
	return err == nil
}

// appleXattrPrefix is the prefix shared by Apple system xattrs that FSKit's
// create flow expects to succeed.
var appleXattrPrefix = []byte("com.apple.")

const (
	opSetXAttr         = uint32(21)
	sizeOfInHeader     = 40
	sizeOfOutHeader    = 16
	setXAttrHeaderSize = 56 // InHeader(40) + Size(4) + Flags(4) + Position(4) + Padding(4)
)

// fskitAdapter handles the FSKit quirk where macOS stamps Apple system xattrs
// (notably com.apple.provenance) on newly created inodes and treats a SETXATTR
// failure as fatal. Filesystems that reject arbitrary xattrs would otherwise
// fail file creation under FSKit, so fskitAdapter tracks in-flight com.apple.*
// SETXATTR requests and masks failing replies to OK.
type fskitAdapter struct {
	mu            sync.Mutex
	appleSetxattr map[uint64]bool
}

func newFSKitAdapter() *fskitAdapter {
	return &fskitAdapter{
		appleSetxattr: make(map[uint64]bool),
	}
}

func hasPrefixAt(iov [][]byte, off int, prefix []byte) bool {
	for _, b := range iov {
		if off >= len(b) {
			off -= len(b)
			continue
		}
		avail := b[off:]
		if len(avail) >= len(prefix) {
			return bytes.HasPrefix(avail, prefix)
		}
		if !bytes.HasPrefix(prefix, avail) {
			return false
		}
		prefix = prefix[len(avail):]
		off = 0
	}
	return false
}

func (a *fskitAdapter) handleInboundIov(in [][]byte) {
	if len(in) == 0 || len(in[0]) < sizeOfInHeader {
		return
	}
	op := binary.LittleEndian.Uint32(in[0][4:8])
	if op != opSetXAttr {
		return
	}
	unique := binary.LittleEndian.Uint64(in[0][8:16])
	if hasPrefixAt(in, setXAttrHeaderSize, appleXattrPrefix) {
		a.mu.Lock()
		a.appleSetxattr[unique] = true
		a.mu.Unlock()
	}
}

func (a *fskitAdapter) shouldMaskSetxattrError(unique uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appleSetxattr[unique] {
		delete(a.appleSetxattr, unique)
		return true
	}
	return false
}

func (a *fskitAdapter) handleOutboundHdr(hdr []byte) {
	if len(hdr) < sizeOfOutHeader {
		return
	}
	unique := binary.LittleEndian.Uint64(hdr[8:16])
	if unique == 0 {
		return
	}
	if a.shouldMaskSetxattrError(unique) {
		if int32(binary.LittleEndian.Uint32(hdr[4:8])) < 0 {
			binary.LittleEndian.PutUint32(hdr[4:8], 0)
		}
	}
}

type responseBuffer struct {
	hdr     [sizeOfOutHeader]byte
	payload []byte
	iov     [][]byte
}

// Server represents an active FSKit file system mount session.
type Server struct {
	rawFS      fuse.RawFileSystem
	ps         *fuse.ProtocolServer
	ch         uintptr
	api        *mfMountAPI
	adapter    *fskitAdapter
	mountPoint string
	opts       *fuse.MountOptions

	bufPool sync.Pool

	closed atomic.Bool
	ready  chan error
	done   chan struct{}
	wg     sync.WaitGroup
}

// Mount mounts rawFS on mountPoint using macOS FSKit (macFUSE 5.3.0+).
// Mount blocks until the file system is successfully mounted and verified by
// macOS, or returns an error.
func Mount(mountPoint string, rawFS fuse.RawFileSystem, opts *fuse.MountOptions) (*Server, error) {
	absMountPoint, err := filepath.Abs(mountPoint)
	if err != nil {
		return nil, err
	}

	api, err := loadMFMountAPI()
	if err != nil {
		return nil, err
	}

	ch := api.channelCreate()
	if ch == 0 {
		return nil, errors.New("fskit: MFChannelCreate failed")
	}

	var optsCopy fuse.MountOptions
	if opts != nil {
		optsCopy = *opts
	}
	optsCopy.DisableSplice = true

	maxPayload := max(optsCopy.MaxWrite, 1<<20)
	s := &Server{
		rawFS:      rawFS,
		ps:         fuse.NewProtocolServer(rawFS, &optsCopy),
		ch:         ch,
		api:        api,
		adapter:    newFSKitAdapter(),
		mountPoint: absMountPoint,
		opts:       &optsCopy,
		ready:      make(chan error, 1),
		done:       make(chan struct{}),
	}

	s.bufPool = sync.Pool{
		New: func() any {
			rb := &responseBuffer{
				payload: make([]byte, maxPayload),
			}
			rb.iov = [][]byte{rb.hdr[:], rb.payload}
			return rb
		},
	}

	go s.serve()

	chRetained := api.retain(ch)
	optsStr := strings.Join(formatOptions(&optsCopy), ",")

	go func() {
		rc := api.mount(chRetained, absMountPoint, optsStr, true)
		api.release(chRetained)
		s.ready <- mapMFMountErr(rc)
		close(s.ready)
	}()

	if mountErr := <-s.ready; mountErr != nil {
		_ = s.Unmount()
		return nil, mountErr
	}

	return s, nil
}

// MountFS mounts a node filesystem on dir using macOS FSKit.
func MountFS(dir string, root fs.InodeEmbedder, options *fs.Options) (*Server, error) {
	rawFS := fs.NewNodeFS(root, options)
	var mountOptions *fuse.MountOptions
	if options != nil {
		mountOptions = &options.MountOptions
	}
	return Mount(dir, rawFS, mountOptions)
}

// ProtocolServer returns the underlying fuse.ProtocolServer instance.
func (s *Server) ProtocolServer() *fuse.ProtocolServer {
	return s.ps
}

// MountPoint returns the absolute path where the filesystem is mounted.
func (s *Server) MountPoint() string {
	return s.mountPoint
}

// Wait blocks until the filesystem is unmounted.
func (s *Server) Wait() {
	<-s.done
}

func (s *Server) closeChannel() {
	if s.closed.CompareAndSwap(false, true) {
		s.api.channelInterrupt(s.ch)
		s.api.channelClose(s.ch)
		s.api.release(s.ch)
	}
}

// Unmount unmounts the filesystem and terminates the server session.
func (s *Server) Unmount() error {
	s.closeChannel()

	// Try graceful unmount first, then force unmount if busy.
	if err := syscall.Unmount(s.mountPoint, 0); err != nil {
		if errForce := syscall.Unmount(s.mountPoint, 1 /* MNT_FORCE */); errForce != nil {
			_ = exec.Command("diskutil", "unmount", "force", s.mountPoint).Run()
		}
	}

	<-s.done
	return nil
}

func (s *Server) serve() {
	defer func() {
		s.wg.Wait()
		s.closeChannel()
		close(s.done)
	}()

	for {
		if s.closed.Load() {
			return
		}

		msg, _, rawErr := purego.SyscallN(s.api.symCopyNextMessage, s.ch)
		if msg == 0 {
			errNo := syscall.Errno(rawErr)
			if errNo == syscall.ENOENT || errNo == syscall.EINTR || errNo == syscall.EAGAIN {
				continue
			}
			return
		}

		s.wg.Add(1)
		go func(m uintptr) {
			defer s.wg.Done()
			s.handle(m)
		}(msg)
	}
}

func (s *Server) handle(msg uintptr) {
	defer s.api.release(msg)

	bodySize := s.api.messageGetBodySize(msg)
	if bodySize < 0 {
		return
	}

	var bufsPtr unsafe.Pointer
	bodyCount := s.api.messageGetBodyBuffers(msg, &bufsPtr)
	if bodyCount < 1 || bufsPtr == nil {
		return
	}

	iovs := unsafe.Slice((*darwinIovec)(bufsPtr), bodyCount)
	var inStack [4][]byte
	var in [][]byte
	if bodyCount <= len(inStack) {
		in = inStack[:bodyCount]
	} else {
		in = make([][]byte, bodyCount)
	}

	for i := 0; i < bodyCount; i++ {
		in[i] = unsafe.Slice((*byte)(iovs[i].base), int(iovs[i].len))
	}

	s.adapter.handleInboundIov(in)

	rb := s.bufPool.Get().(*responseBuffer)
	defer s.bufPool.Put(rb)
	clear(rb.hdr[:])

	n, _ := s.ps.HandleRequest(in, rb.iov)
	if n <= 0 {
		return
	}

	s.adapter.handleOutboundHdr(rb.hdr[:])
	s.sendMessage(rb.hdr[:], rb.payload, n)
}

func (s *Server) sendMessage(outHdr, outPayload []byte, n int) {
	if s.closed.Load() {
		return
	}

	var stackIovs [2]darwinIovec
	diovs := stackIovs[:0]

	hdrLen := min(n, sizeOfOutHeader)
	diovs = append(diovs, darwinIovec{
		base: unsafe.Pointer(&outHdr[0]),
		len:  uint64(hdrLen),
	})

	if n > sizeOfOutHeader {
		payloadLen := n - sizeOfOutHeader
		diovs = append(diovs, darwinIovec{
			base: unsafe.Pointer(&outPayload[0]),
			len:  uint64(payloadLen),
		})
	}

	r1, _, rawErr := purego.SyscallN(s.api.symSendMessage, s.ch, uintptr(unsafe.Pointer(&diovs[0])), uintptr(len(diovs)))
	res := int(r1)
	if res < 0 {
		errNo := syscall.Errno(rawErr)
		if errNo == syscall.ENOENT {
			return
		}
		if s.opts.Debug {
			s.opts.Logger.Printf("fskit: MFChannelSendMessage error: %v", errNo)
		}
	}
}

func formatOptions(o *fuse.MountOptions) []string {
	var r []string
	r = append(r, o.Options...)

	if o.AllowOther {
		r = append(r, "allow_other")
	}
	if o.FsName != "" {
		r = append(r, "fsname="+o.FsName)
	}
	if o.Name != "" {
		r = append(r, "subtype="+o.Name)
	}
	if o.MaxWrite > 0 {
		r = append(r, fmt.Sprintf("max_read=%d", o.MaxWrite))
		r = append(r, fmt.Sprintf("iosize=%d", o.MaxWrite))
	}
	r = append(r, "daemon_timeout=0")

	var rEscaped []string
	for _, s := range r {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `,`, `\,`)
		rEscaped = append(rEscaped, s)
	}
	return rEscaped
}
