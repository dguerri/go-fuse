package fs

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// fskitOptions returns *Options preconfigured to mimic an FSKit mount.
func fskitOptions() *Options {
	o := &Options{}
	o.MountOptions.Backend = "fskit"
	return o
}

func TestFSKitBridge_NodeIdEqualsIno(t *testing.T) {
	root := &Inode{}
	rawFS := NewNodeFS(root, fskitOptions()).(*rawBridge)

	child := rawFS.newInodeUnlocked(&Inode{}, StableAttr{Ino: 42, Mode: fuse.S_IFREG}, true)
	if child.nodeId != 42 {
		t.Fatalf("nodeId = %d, want 42", child.nodeId)
	}
	if child.stableAttr.Ino != 42 {
		t.Fatalf("Ino = %d, want 42", child.stableAttr.Ino)
	}
}

func TestFSKitBridge_RejectsZeroIno(t *testing.T) {
	root := &Inode{}
	rawFS := NewNodeFS(root, fskitOptions()).(*rawBridge)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for Ino==0 in FSKit mode")
		}
		if !strings.Contains(panicString(r), "Ino==0") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	rawFS.newInodeUnlocked(&Inode{}, StableAttr{Ino: 0, Mode: fuse.S_IFREG}, true)
}

func TestFSKitBridge_RejectsRootInoForNonRoot(t *testing.T) {
	root := &Inode{}
	rawFS := NewNodeFS(root, fskitOptions()).(*rawBridge)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for Ino==FUSE_ROOT_ID for non-root in FSKit mode")
		}
		if !strings.Contains(panicString(r), "FUSE_ROOT_ID") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	rawFS.newInodeUnlocked(&Inode{}, StableAttr{Ino: fuse.FUSE_ROOT_ID, Mode: fuse.S_IFREG}, true)
}

func TestClassicBridge_SequentialNodeIds(t *testing.T) {
	root := &Inode{}
	rawFS := NewNodeFS(root, &Options{}).(*rawBridge)

	a := rawFS.newInodeUnlocked(&Inode{}, StableAttr{Ino: 100, Mode: fuse.S_IFREG}, true)
	b := rawFS.newInodeUnlocked(&Inode{}, StableAttr{Ino: 200, Mode: fuse.S_IFREG}, true)
	if a.nodeId == a.stableAttr.Ino || b.nodeId == b.stableAttr.Ino {
		t.Fatalf("classic mode should not couple nodeId to Ino: a.nodeId=%d a.Ino=%d b.nodeId=%d b.Ino=%d",
			a.nodeId, a.stableAttr.Ino, b.nodeId, b.stableAttr.Ino)
	}
	if b.nodeId != a.nodeId+1 {
		t.Fatalf("expected sequential nodeIds, got a=%d b=%d", a.nodeId, b.nodeId)
	}
}

func TestClassicBridge_NodeIdZeroPanics(t *testing.T) {
	root := &Inode{}
	rawFS := NewNodeFS(root, &Options{}).(*rawBridge)

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nid==0 in classic mode")
		}
	}()
	rawFS.inode(0, 0)
}

type enoentLookuper struct {
	Inode
}

func (n *enoentLookuper) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*Inode, syscall.Errno) {
	return nil, syscall.ENOENT
}

var _ NodeLookuper = (*enoentLookuper)(nil)

func TestFSKitBridge_NegativeLookupReturnsENOENT(t *testing.T) {
	root := &enoentLookuper{}
	neg := time.Second
	opts := fskitOptions()
	opts.NegativeTimeout = &neg
	rawFS := NewNodeFS(root, opts).(*rawBridge)

	header := &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}
	var out fuse.EntryOut
	st := rawFS.Lookup(make(<-chan struct{}), header, "missing", &out)
	if st != fuse.Status(syscall.ENOENT) {
		t.Fatalf("status = %v, want ENOENT (FSKit mode must not emit negative-entry replies)", st)
	}
}

func TestClassicBridge_NegativeLookupCachedAsNodeIDZero(t *testing.T) {
	root := &enoentLookuper{}
	neg := time.Second
	opts := &Options{NegativeTimeout: &neg}
	rawFS := NewNodeFS(root, opts).(*rawBridge)

	header := &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}
	var out fuse.EntryOut
	st := rawFS.Lookup(make(<-chan struct{}), header, "missing", &out)
	if !st.Ok() {
		t.Fatalf("status = %v, want OK (classic mode caches negative entry as NodeId==0)", st)
	}
	if out.NodeId != 0 {
		t.Fatalf("NodeId = %d, want 0", out.NodeId)
	}
}

// panicString extracts a string representation from a recovered panic value.
func panicString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		return ""
	}
}
