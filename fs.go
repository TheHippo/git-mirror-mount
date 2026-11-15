package main

import (
	"container/list"
	"context"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Design notes
//
// Every inode below the root(s) is immutable: a directory inode stands for one
// tree object at one position with one mtime, a file inode for one blob at one
// path with one mtime. When the branch moves, a new snapshot becomes current;
// only the root node (layout "tree") or the "current" symlink (layout
// "snapshots") changes, and is invalidated in the kernel via NotifyEntry.
// Everything else is reached through new (or, if unchanged, the same)
// immutable inodes. That allows long kernel cache timeouts and keeps the page
// cache for unchanged files.
//
// mtimes are deterministic: every path starts at the commit time of the first
// mounted commit; on each refresh the paths reported by `git diff-tree`
// (files and the directories containing them) get the new commit's time.

type gitFS struct {
	pool     *pool
	gitDir   string
	rev      string
	hashLen  int
	blobs    *blobCache
	maxInMem int64
	spools   *spoolMgr
	lfsDir   string // "" = LFS resolution disabled
	pinRef   string // "" = don't pin

	cur       atomic.Pointer[snapshot]
	prev      string // previously mounted commit (for pinning)
	refreshMu sync.Mutex
	onSwitch  func(old, cur *snapshot, diff []diffEntry)

	seenMu sync.Mutex
	seen   map[string]*snapshot // commits reachable under commits/ (snapshots layout)
}

type snapshot struct {
	g     *gitFS
	info  commitInfo
	base  time.Time
	times map[string]time.Time // per-path overrides of base; read-only once published

	once sync.Once
	list *treeList
	err  error
}

func (s *snapshot) mtime(p string) time.Time {
	if t, ok := s.times[p]; ok {
		return t
	}
	return s.base
}

func (s *snapshot) root() (*treeList, error) {
	s.once.Do(func() { s.list, s.err = s.g.loadTree(s.info.tree) })
	return s.list, s.err
}

type treeList struct {
	entries []treeEntry
	byName  map[string]int
}

func (g *gitFS) loadTree(oid string) (*treeList, error) {
	entries, err := g.pool.readTree(oid, g.hashLen, g.lfsDir)
	if err != nil {
		return nil, err
	}
	tl := &treeList{entries: entries, byName: make(map[string]int, len(entries))}
	for i, e := range entries {
		tl.byName[e.name] = i
	}
	return tl, nil
}

// refresh re-resolves the branch and, if it moved, publishes a new snapshot.
func (g *gitFS) refresh() error {
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	info, err := resolve(g.gitDir, g.rev)
	if err != nil {
		return err
	}
	old := g.cur.Load()
	if old != nil && old.info.commit == info.commit {
		g.pin() // re-create pins a `fetch --prune` may have removed
		return nil
	}
	g.pool.reset() // fresh processes see newly fetched packs
	s := &snapshot{g: g, info: info}
	var diff []diffEntry
	if old == nil {
		s.base, s.times = info.time, map[string]time.Time{}
	} else {
		diff, err = diffTrees(g.gitDir, old.info.tree, info.tree)
		if err != nil {
			return err
		}
		s.base = old.base
		s.times = make(map[string]time.Time, len(old.times)+len(diff)+1)
		for k, v := range old.times {
			s.times[k] = v
		}
		if old.info.tree != info.tree {
			s.times[""] = info.time
		}
		for _, d := range diff {
			s.times[d.path] = info.time
		}
	}
	if _, err := s.root(); err != nil {
		return err
	}
	g.cur.Store(s)
	g.seenMu.Lock()
	g.seen[info.commit] = s
	g.seenMu.Unlock()
	if old != nil {
		g.prev = old.info.commit
		log.Printf("%s moved %.12s -> %.12s (%d paths changed)", g.rev, old.info.commit, info.commit, len(diff))
		if g.onSwitch != nil {
			g.onSwitch(old, s, diff)
		}
		g.prefetch(diff)
	}
	g.pin()
	return nil
}

func (g *gitFS) pin() {
	if g.pinRef == "" {
		return
	}
	if err := ensureRef(g.gitDir, g.pinRef+"/current", g.cur.Load().info.commit); err != nil {
		log.Printf("pin: %v", err)
	}
	if g.prev != "" {
		if err := ensureRef(g.gitDir, g.pinRef+"/previous", g.prev); err != nil {
			log.Printf("pin: %v", err)
		}
	}
}

// prefetch puts large changed blobs into the disk cache after a refresh.
func (g *gitFS) prefetch(diff []diffEntry) {
	if g.spools.cache == nil {
		return
	}
	var oids []string
	for _, d := range diff {
		if d.newMode == modeBlob || d.newMode == modeExec {
			oids = append(oids, d.newOid)
		}
	}
	if len(oids) == 0 {
		return
	}
	var sizes []int64
	err := g.pool.with(func(c *catFile) (err error) {
		sizes, err = c.infoMany(oids)
		return err
	})
	if err != nil {
		log.Printf("prefetch: %v", err)
		return
	}
	var big []string
	var bigSizes []int64
	for i, sz := range sizes {
		if sz > g.maxInMem && sz <= g.spools.cache.limit/2 {
			big = append(big, oids[i])
			bigSizes = append(bigSizes, sz)
		}
	}
	g.spools.prefetch(big, bigSizes)
}

// ---------------------------------------------------------------------------
// Inode numbering. lookupIn and dirStream must agree, so both use childIno.

func inoHash(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	v := h.Sum64()
	if v <= 1 || v == ^uint64(0) { // 0, 1 (root) and ^0 are reserved
		v += 2
	}
	return v
}

func u64s(v uint64) string {
	var b [8]byte
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return string(b[:])
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func childIno(parentIno uint64, parentPath string, e treeEntry, s *snapshot) uint64 {
	p := join(parentPath, e.name)
	mt := u64s(uint64(s.mtime(p).UnixNano()))
	switch e.mode {
	case modeTree, modeGitlink:
		// Unique per parent inode: the kernel must never see a directory
		// under two parents.
		return inoHash("d", u64s(parentIno), e.name, e.oid, mt)
	case modeSymlink:
		return inoHash("l", p, e.oid, mt)
	default:
		// Same path + blob + mode + mtime => same inode across snapshots,
		// which keeps the kernel page cache warm after a refresh.
		return inoHash("f", p, e.oid, u64s(uint64(e.mode)), mt)
	}
}

func fileMode(m uint32) uint32 {
	switch m {
	case modeTree, modeGitlink:
		return syscall.S_IFDIR | 0o555
	case modeSymlink:
		return syscall.S_IFLNK | 0o777
	case modeExec:
		return syscall.S_IFREG | 0o555
	default:
		return syscall.S_IFREG | 0o444
	}
}

func setTimes(a *fuse.Attr, t time.Time) {
	a.SetTimes(&t, &t, &t)
}

func (g *gitFS) lookupIn(ctx context.Context, parent *fs.Inode, parentPath string, tl *treeList, s *snapshot, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	i, ok := tl.byName[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	e := tl.entries[i]
	p := join(parentPath, e.name)
	mt := s.mtime(p)
	pIno := parent.StableAttr().Ino
	mode := fileMode(e.mode)
	var ops fs.InodeEmbedder
	switch e.mode {
	case modeTree, modeGitlink:
		ops = &dirNode{g: g, path: p, oid: e.oid, gitlink: e.mode == modeGitlink, snap: s, parentIno: pIno, mtime: mt}
	case modeSymlink:
		ops = &symlinkNode{g: g, oid: e.oid, size: e.size, mtime: mt}
	default:
		ops = &fileNode{g: g, oid: e.oid, size: e.size, mode: mode, mtime: mt, lfs: e.lfs}
	}
	child := parent.NewInode(ctx, ops, fs.StableAttr{Mode: mode, Ino: childIno(pIno, parentPath, e, s)})
	fillEntry(ctx, child, out)
	return child, 0
}

func fillEntry(ctx context.Context, n *fs.Inode, out *fuse.EntryOut) {
	if ga, ok := n.Operations().(fs.NodeGetattrer); ok {
		var ao fuse.AttrOut
		ga.Getattr(ctx, nil, &ao)
		out.Attr = ao.Attr
	}
}

func dotEntries(self, parent uint64) []fuse.DirEntry {
	return []fuse.DirEntry{
		{Name: ".", Mode: syscall.S_IFDIR, Ino: self},
		{Name: "..", Mode: syscall.S_IFDIR, Ino: parent},
	}
}

func dirStream(selfIno, parentIno uint64, path string, tl *treeList, s *snapshot) fs.DirStream {
	list := make([]fuse.DirEntry, 0, len(tl.entries)+2)
	list = append(list, dotEntries(selfIno, parentIno)...)
	for _, e := range tl.entries {
		list = append(list, fuse.DirEntry{Name: e.name, Mode: fileMode(e.mode), Ino: childIno(selfIno, path, e, s)})
	}
	return fs.NewListDirStream(list)
}

// ---------------------------------------------------------------------------
// Layout "tree": the mount root is the branch's tree.

type treeRoot struct {
	fs.Inode
	g *gitFS
}

var (
	_ fs.NodeGetattrer = (*treeRoot)(nil)
	_ fs.NodeLookuper  = (*treeRoot)(nil)
	_ fs.NodeReaddirer = (*treeRoot)(nil)
)

func newTreeRoot(g *gitFS) *treeRoot {
	r := &treeRoot{g: g}
	g.onSwitch = func(old, cur *snapshot, diff []diffEntry) {
		// Only top-level names need invalidating; deeper paths hang off
		// immutable inodes.
		for _, d := range diff {
			if !strings.Contains(d.path, "/") {
				r.NotifyEntry(d.path) // ENOENT: kernel had nothing cached
			}
		}
		r.NotifyContent(0, 0)
	}
	return r
}

func (r *treeRoot) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	s := r.g.cur.Load()
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	setTimes(&out.Attr, s.mtime(""))
	out.SetTimeout(time.Second)
	return 0
}

func (r *treeRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	s := r.g.cur.Load()
	tl, err := s.root()
	if err != nil {
		return nil, syscall.EIO
	}
	return r.g.lookupIn(ctx, r.EmbeddedInode(), "", tl, s, name, out)
}

func (r *treeRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	s := r.g.cur.Load()
	tl, err := s.root()
	if err != nil {
		return nil, syscall.EIO
	}
	return dirStream(fuse.FUSE_ROOT_ID, fuse.FUSE_ROOT_ID, "", tl, s), 0
}

// ---------------------------------------------------------------------------
// Layout "snapshots":
//
//	current -> commits/<sha>     (switches atomically on refresh)
//	commits/<sha>/...            (every commit mounted this session, plus any
//	                              full commit id you look up)

type snapRoot struct {
	fs.Inode
	g *gitFS
}

var (
	_ fs.NodeGetattrer = (*snapRoot)(nil)
	_ fs.NodeLookuper  = (*snapRoot)(nil)
	_ fs.NodeReaddirer = (*snapRoot)(nil)
)

var commitsIno = inoHash("commits")

func currentIno(commit string) uint64 { return inoHash("current", commit) }
func commitIno(commit string) uint64  { return inoHash("c", commit) }

func newSnapRoot(g *gitFS) *snapRoot {
	r := &snapRoot{g: g}
	g.onSwitch = func(old, cur *snapshot, diff []diffEntry) {
		r.NotifyEntry("current")
		r.NotifyContent(0, 0)
	}
	return r
}

func (r *snapRoot) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 3
	setTimes(&out.Attr, r.g.cur.Load().info.time)
	out.SetTimeout(time.Second)
	return 0
}

func (r *snapRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	switch name {
	case "current":
		s := r.g.cur.Load()
		n := r.NewInode(ctx, &linkNode{target: "commits/" + s.info.commit, mtime: s.info.time},
			fs.StableAttr{Mode: syscall.S_IFLNK, Ino: currentIno(s.info.commit)})
		fillEntry(ctx, n, out)
		return n, 0
	case "commits":
		n := r.NewInode(ctx, &commitsDir{g: r.g}, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: commitsIno})
		fillEntry(ctx, n, out)
		return n, 0
	}
	return nil, syscall.ENOENT
}

func (r *snapRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	s := r.g.cur.Load()
	list := append(dotEntries(fuse.FUSE_ROOT_ID, fuse.FUSE_ROOT_ID),
		fuse.DirEntry{Name: "commits", Mode: syscall.S_IFDIR, Ino: commitsIno},
		fuse.DirEntry{Name: "current", Mode: syscall.S_IFLNK, Ino: currentIno(s.info.commit)})
	return fs.NewListDirStream(list), 0
}

type commitsDir struct {
	fs.Inode
	g *gitFS
}

var (
	_ fs.NodeGetattrer = (*commitsDir)(nil)
	_ fs.NodeLookuper  = (*commitsDir)(nil)
	_ fs.NodeReaddirer = (*commitsDir)(nil)
)

func (c *commitsDir) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	setTimes(&out.Attr, c.g.cur.Load().info.time)
	out.SetTimeout(time.Second)
	return 0
}

func (c *commitsDir) snapshotFor(name string) *snapshot {
	g := c.g
	g.seenMu.Lock()
	s := g.seen[name]
	g.seenMu.Unlock()
	if s != nil || len(name) != 2*g.hashLen || !isHex(name) {
		return s
	}
	info, err := resolve(g.gitDir, name)
	if err != nil || info.commit != name {
		return nil
	}
	// A commit outside the mounted timeline: all mtimes = its commit time.
	s = &snapshot{g: g, info: info, base: info.time, times: map[string]time.Time{}}
	g.seenMu.Lock()
	if prior := g.seen[name]; prior != nil {
		s = prior
	} else {
		g.seen[name] = s
	}
	g.seenMu.Unlock()
	return s
}

func (c *commitsDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	s := c.snapshotFor(name)
	if s == nil {
		// Not cached as negative entry: the commit may arrive with the next fetch.
		out.SetEntryTimeout(time.Nanosecond)
		return nil, syscall.ENOENT
	}
	n := c.NewInode(ctx, &dirNode{g: c.g, oid: s.info.tree, snap: s, parentIno: commitsIno, mtime: s.mtime("")},
		fs.StableAttr{Mode: syscall.S_IFDIR, Ino: commitIno(s.info.commit)})
	fillEntry(ctx, n, out)
	return n, 0
}

func (c *commitsDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	c.g.seenMu.Lock()
	names := make([]string, 0, len(c.g.seen))
	for k := range c.g.seen {
		names = append(names, k)
	}
	c.g.seenMu.Unlock()
	sort.Strings(names)
	list := dotEntries(commitsIno, fuse.FUSE_ROOT_ID)
	for _, n := range names {
		list = append(list, fuse.DirEntry{Name: n, Mode: syscall.S_IFDIR, Ino: commitIno(n)})
	}
	return fs.NewListDirStream(list), 0
}

type linkNode struct {
	fs.Inode
	target string
	mtime  time.Time
}

var (
	_ fs.NodeGetattrer  = (*linkNode)(nil)
	_ fs.NodeReadlinker = (*linkNode)(nil)
)

func (l *linkNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFLNK | 0o777
	out.Nlink = 1
	out.Size = uint64(len(l.target))
	setTimes(&out.Attr, l.mtime)
	return 0
}

func (l *linkNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return []byte(l.target), 0
}

// ---------------------------------------------------------------------------
// Directories (trees) and submodules (gitlinks, shown as empty dirs).

type dirNode struct {
	fs.Inode
	g         *gitFS
	path      string // relative to the commit root, "" for the root itself
	oid       string
	gitlink   bool
	snap      *snapshot
	parentIno uint64
	mtime     time.Time

	once sync.Once
	list *treeList
	err  error
}

var (
	_ fs.NodeGetattrer = (*dirNode)(nil)
	_ fs.NodeLookuper  = (*dirNode)(nil)
	_ fs.NodeReaddirer = (*dirNode)(nil)
)

func (d *dirNode) load() (*treeList, error) {
	d.once.Do(func() {
		if d.gitlink {
			d.list = &treeList{byName: map[string]int{}}
			return
		}
		d.list, d.err = d.g.loadTree(d.oid)
		if d.err != nil {
			log.Printf("reading tree %s (%s): %v", d.oid, d.path, d.err)
		}
	})
	return d.list, d.err
}

func (d *dirNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	setTimes(&out.Attr, d.mtime)
	return 0
}

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	tl, err := d.load()
	if err != nil {
		return nil, syscall.EIO
	}
	return d.g.lookupIn(ctx, d.EmbeddedInode(), d.path, tl, d.snap, name, out)
}

func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	tl, err := d.load()
	if err != nil {
		return nil, syscall.EIO
	}
	return dirStream(d.StableAttr().Ino, d.parentIno, d.path, tl, d.snap), 0
}

// ---------------------------------------------------------------------------
// Symlinks

type symlinkNode struct {
	fs.Inode
	g     *gitFS
	oid   string
	size  int64
	mtime time.Time
}

var (
	_ fs.NodeGetattrer  = (*symlinkNode)(nil)
	_ fs.NodeReadlinker = (*symlinkNode)(nil)
)

func (s *symlinkNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFLNK | 0o777
	out.Nlink = 1
	out.Size = uint64(s.size)
	setTimes(&out.Attr, s.mtime)
	return 0
}

func (s *symlinkNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	b, err := s.g.pool.readObject(s.oid, "blob")
	if err != nil {
		log.Printf("readlink %s: %v", s.oid, err)
		return nil, syscall.EIO
	}
	return b, 0
}

// ---------------------------------------------------------------------------
// Regular files. Nothing is read on open(); the first Read decides how:
//   - LFS object present locally: read that file directly
//   - small blob: whole blob in memory (shared LRU)
//   - large blob: streamed into a spool file; reads wait only for their range

type fileNode struct {
	fs.Inode
	g     *gitFS
	oid   string
	size  int64
	mode  uint32
	mtime time.Time
	lfs   string
}

var (
	_ fs.NodeGetattrer = (*fileNode)(nil)
	_ fs.NodeOpener    = (*fileNode)(nil)
)

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = f.mode
	out.Nlink = 1
	out.Size = uint64(f.size)
	out.Blocks = (uint64(f.size) + 511) / 512
	out.Blksize = 4096
	setTimes(&out.Attr, f.mtime)
	return 0
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
		return nil, 0, syscall.EROFS
	}
	// This inode's content can never change: let the kernel keep its page
	// cache across opens.
	return &fileHandle{n: f}, fuse.FOPEN_KEEP_CACHE, 0
}

type fileHandle struct {
	n    *fileNode
	once sync.Once
	data []byte
	sp   *spool
	f    *os.File
	err  error
}

var (
	_ fs.FileReader   = (*fileHandle)(nil)
	_ fs.FileReleaser = (*fileHandle)(nil)
)

func (h *fileHandle) load() {
	n := h.n
	switch {
	case n.lfs != "":
		h.f, h.err = os.Open(n.lfs)
	case n.size <= n.g.maxInMem:
		h.data, h.err = n.g.blobs.get(n.oid, func() ([]byte, error) {
			return n.g.pool.readObject(n.oid, "blob")
		})
	default:
		h.sp, h.err = n.g.spools.acquire(n.oid, n.size)
	}
	if h.err != nil {
		log.Printf("open blob %s: %v", n.oid, h.err)
	}
}

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.once.Do(h.load)
	if h.err != nil {
		return nil, syscall.EIO
	}
	switch {
	case h.f != nil:
		n, err := h.f.ReadAt(dest, off)
		if err != nil && err != io.EOF {
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(dest[:n]), 0
	case h.sp != nil:
		n, err := h.sp.readAt(dest, off)
		if err != nil {
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(dest[:n]), 0
	}
	if off >= int64(len(h.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(h.data)) {
		end = int64(len(h.data))
	}
	return fuse.ReadResultData(h.data[off:end]), 0
}

func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	h.once.Do(func() {}) // never read: nothing to release
	if h.sp != nil {
		h.n.g.spools.release(h.sp)
	}
	if h.f != nil {
		h.f.Close()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Small LRU cache of blob contents, bounded by total bytes.

type blobCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	ll     *list.List
	items  map[string]*list.Element
}

type blobItem struct {
	oid  string
	data []byte
}

func newBlobCache(budget int64) *blobCache {
	return &blobCache{budget: budget, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *blobCache) get(oid string, load func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.items[oid]; ok {
		c.ll.MoveToFront(el)
		data := el.Value.(*blobItem).data
		c.mu.Unlock()
		return data, nil
	}
	c.mu.Unlock()

	data, err := load()
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.budget {
		return data, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[oid]; !ok {
		c.items[oid] = c.ll.PushFront(&blobItem{oid: oid, data: data})
		c.used += int64(len(data))
		for c.used > c.budget {
			el := c.ll.Back()
			it := el.Value.(*blobItem)
			c.ll.Remove(el)
			delete(c.items, it.oid)
			c.used -= int64(len(it.data))
		}
	}
	return data, nil
}

// lfsObjectsDir returns <gitDir>/lfs/objects.
func lfsObjectsDir(gitDir string) string { return filepath.Join(gitDir, "lfs", "objects") }
