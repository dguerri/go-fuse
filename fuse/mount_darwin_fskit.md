# FSKit backend: design notes

This document explains why the FSKit-backed mount path
(`MountOptions.Backend == "fskit"`) requires bridge changes that the kext-backed
path does not, and what the trade-offs are. It is intended for go-fuse
contributors and for upstream review.

## What FSKit does differently from FUSE

**Classic FUSE (Linux kernel module, legacy macFUSE kext).** Userspace mints
an opaque `NodeId` and returns it to the kernel via `EntryOut` after
`LOOKUP`/`CREATE`/etc. The kernel echoes it back on every subsequent op.
`Attr.Ino` is separate — it is only the value reported to userspace via
`stat()` (i.e., `st_ino`) and is not used by the kernel as an addressing
handle. Userspace is free to pick any `NodeId` scheme; go-fuse normally
picks sequential opaque IDs starting at 2.

**FSKit (macFUSE 5.2+ on macOS 15.4+).** FSKit is Apple's in-kernel
filesystem framework, sitting *above* macFUSE. Its API is built around
`FSItem`, where each item carries an `identifier` chosen by the filesystem,
conventionally the inode number. macFUSE's FSKit shim translates FSKit calls
into FUSE messages and uses whatever `Attr.Ino` the userspace filesystem
published as the `NodeId` slot on the wire. Net effect: the userspace
filesystem does not get to choose its addressing space — FSKit imposes it.

There is also no `LOOKUP`-before-`GETATTR` in FSKit's data model.
`enumerateDirectory` returns `FSItem`s directly and FSKit treats their
identifiers as authoritative handles, asking for attributes on each item
without a separate lookup-by-name-in-parent step. The macFUSE shim
translates this as: `READDIR` reply yields a list of Inos → FSKit issues
`getAttributes(N)` → shim sends `FUSE_GETATTR` with `NodeId=N`. The
userspace bridge has never been told to register a node for that Ino, so
without intervention `kernelNodeIds[N]` is nil and the bridge panics.

## What this code does about it

When `MountOptions.Backend == "fskit"`:

1. **The bridge mints `nodeId == StableAttr.Ino`** (see
   `fs/bridge.go:newInodeUnlocked`). This makes `kernelNodeIds[Ino]` the
   single source of truth; FSKit's "address by Ino" convention and the
   bridge's identity coincide. The filesystem's contract becomes: every
   node must publish a non-zero, non-`FUSE_ROOT_ID`-for-non-root,
   non-reserved `Ino`. The bridge panics at the source if the contract is
   violated.

2. **`nid==0` is rewritten to `FUSE_ROOT_ID` at the wire**
   (see `mount_darwin_fskit_adapter.go:handleInbound`). FSKit occasionally
   addresses the volume root with `nodeid=0` (e.g., an extra `GETATTR` after
   a successful `nodeid=1` probe, or a `ReleaseDir` on a handle opened
   against `nodeid=0`). Rewriting once at wire intake covers every bridge
   entry point uniformly; classic FUSE never sees the rewrite and still
   panics on an unknown `nodeid=0`.

3. **Negative-entry caching is disabled** (see `fs/bridge.go:Lookup`). FUSE
   encodes "name doesn't exist, please cache the absence" as a successful
   `LOOKUP` reply with `NodeId==0`. FSKit reads `Ino==0` as "this is the
   volume root" and routes follow-ups at the parent. In FSKit mode the
   bridge returns `ENOENT` instead.

4. **The FSKit adapter promotes `READDIR` → `READDIRPLUS` on the wire**
   (see `mount_darwin_fskit_adapter.go`). This forces the bridge to call
   `Lookup()` per child during the directory enumeration so each child is
   registered in `kernelNodeIds` before FSKit can issue its dependent
   `GETATTR`. The reply is transformed back to plain `READDIR` shape.

5. **The FSKit adapter masks `com.apple.*` SETXATTR failures**
   (see `mount_darwin_fskit_adapter.go`). FSKit's CREATE flow stamps system
   xattrs (notably `com.apple.provenance`) on new inodes and treats
   `SETXATTR` failure as fatal. Filesystems that do not accept arbitrary
   xattrs would otherwise be silently rendered read-only under FSKit.

## Trade-offs of `NodeId == Ino`

go-fuse's normal design separates `NodeId` from `Ino` for these reasons:

1. *Decoupling lifecycle from the filesystem's Ino space.* `NodeId` is a
   FUSE-protocol identity tied to FORGET/lookup-count bookkeeping inside
   the bridge. Keeping it separate lets the bridge own its own lifecycle
   and not trust the filesystem to give it unique, stable, non-reserved
   values.
2. *Avoids `Ino==0` / `Ino==1` collisions.* `0` means "no entry" in FUSE;
   `1` is `FUSE_ROOT_ID`.
3. *Robustness to lazy or buggy filesystems* that return `Ino=0` universally
   or reuse Inos after delete+create.
4. *Generation-free operation.* Monotonic `NodeId`s let the bridge leave
   `Generation==0` and avoid stale-handle bugs entirely; `NodeId==Ino`
   requires real `Generation` handling if Inos are reused.
5. *Convention.* libfuse's high-level API and most FUSE bindings use
   sequential opaque IDs.

In FSKit mode each of these costs is pushed up to the filesystem
implementer: the filesystem must publish unique, stable, non-reserved Inos
and must not reuse them after FORGET (or must bump `Generation` if it
does). The bridge enforces what it can at node creation
(`Ino==0` / `Ino==FUSE_ROOT_ID`-for-non-root panic).

The alternative — keeping the bridge's NodeId space separate and translating
in the adapter — was the original design and was abandoned because it
produced two maps protected by different mutexes with different lifecycle
rules, which cannot be kept consistent under concurrent FUSE traffic.
Coverage gaps (any opcode whose reply layout the adapter doesn't parse
leaks an unregistered Ino), best-effort `BATCH_FORGET` cleanup, and
lookup-count/Ino lifecycle skew each produced `unknown node N` panics. A
single map keyed by the same value FSKit sends us removes the entire
class of bugs.

## Why this is a workaround, not a final design

Both the addressing-by-Ino convention and the missing pre-`LOOKUP` are
properties of macFUSE's FSKit shim, not of FSKit itself or of FUSE. A more
complete shim could mint its own opaque `NodeId` space and issue
`LOOKUP`s on demand to maintain FUSE-equivalent semantics for unmodified
filesystems. When (if) macFUSE ships such a shim, the entire FSKit adapter
and every `Backend == "fskit"` branch in the bridge should delete cleanly.
