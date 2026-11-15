package main

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A spool is a large blob being streamed (or already streamed) from git into
// a file. Readers block only until the bytes they ask for have arrived, so
// sequential readers get data immediately instead of after the whole blob
// has been inflated.
type spool struct {
	oid  string
	size int64
	f    *os.File

	mu      sync.Mutex
	cond    *sync.Cond
	written int64
	done    bool
	err     error
	refs    int // guarded by spoolMgr.mu
}

func newSpool(oid string, size int64, f *os.File) *spool {
	s := &spool{oid: oid, size: size, f: f}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *spool) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	s.mu.Lock()
	s.written += int64(n)
	s.cond.Broadcast()
	s.mu.Unlock()
	return n, err
}

func (s *spool) finish(err error) {
	s.mu.Lock()
	s.done = true
	s.err = err
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *spool) readAt(dest []byte, off int64) (int, error) {
	if off >= s.size {
		return 0, nil
	}
	end := off + int64(len(dest))
	if end > s.size {
		end = s.size
	}
	s.mu.Lock()
	for !s.done && s.written < end {
		s.cond.Wait()
	}
	avail, err := s.written, s.err
	s.mu.Unlock()
	if avail < end {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	n, err := s.f.ReadAt(dest[:end-off], off)
	if err == io.EOF && int64(n) == end-off {
		err = nil
	}
	return n, err
}

// spoolMgr shares one spool per blob between concurrent opens and backs
// spools with the optional disk cache.
type spoolMgr struct {
	pool   *pool
	cache  *diskCache // nil: no disk cache, spool into unlinked temp files
	tmpDir string

	mu     sync.Mutex
	active map[string]*spool
}

func newSpoolMgr(p *pool, cache *diskCache, tmpDir string) *spoolMgr {
	return &spoolMgr{pool: p, cache: cache, tmpDir: tmpDir, active: map[string]*spool{}}
}

func (m *spoolMgr) acquire(oid string, size int64) (*spool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.active[oid]; ok {
		s.refs++
		return s, nil
	}
	if m.cache != nil {
		if path, ok := m.cache.lookup(oid); ok {
			if f, err := os.Open(path); err == nil {
				s := newSpool(oid, size, f)
				s.written, s.done, s.refs = size, true, 1
				m.active[oid] = s
				return s, nil
			}
		}
	}
	var f *os.File
	var err error
	if m.cache != nil {
		f, err = m.cache.create(oid)
	} else {
		f, err = os.CreateTemp(m.tmpDir, "git-mirror-mount-*")
		if err == nil {
			os.Remove(f.Name())
		}
	}
	if err != nil {
		return nil, err
	}
	s := newSpool(oid, size, f)
	s.refs = 2 // caller + filler goroutine
	m.active[oid] = s
	go m.fill(s)
	return s, nil
}

func (m *spoolMgr) fill(s *spool) {
	err := m.pool.with(func(c *catFile) error {
		_, _, err := c.contents(s.oid, s)
		return err
	})
	if m.cache != nil {
		if err == nil {
			err = m.cache.commit(s.oid, s.f.Name(), s.size)
		} else {
			os.Remove(s.f.Name())
		}
	}
	s.finish(err)
	if err != nil {
		log.Printf("streaming blob %s: %v", s.oid, err)
		m.mu.Lock()
		if m.active[s.oid] == s {
			delete(m.active, s.oid) // next open retries
		}
		m.mu.Unlock()
	}
	m.release(s)
}

func (m *spoolMgr) release(s *spool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.refs--
	if s.refs == 0 {
		if m.active[s.oid] == s {
			delete(m.active, s.oid)
		}
		s.f.Close()
	}
}

// prefetch streams blobs into the disk cache in the background, one at a
// time, so the first open after a fetch is fast.
func (m *spoolMgr) prefetch(oids []string, sizes []int64) {
	if m.cache == nil || len(oids) == 0 {
		return
	}
	go func() {
		for i, oid := range oids {
			if _, ok := m.cache.lookup(oid); ok {
				continue
			}
			s, err := m.acquire(oid, sizes[i])
			if err != nil {
				log.Printf("prefetch %s: %v", oid, err)
				continue
			}
			s.mu.Lock()
			for !s.done {
				s.cond.Wait()
			}
			s.mu.Unlock()
			m.release(s)
		}
	}()
}

// ---------------------------------------------------------------------------
// diskCache: content-addressed, so entries never need invalidation. Evicts
// least recently used files beyond the size limit.

type diskCache struct {
	dir   string
	limit int64

	mu    sync.Mutex
	total int64
	items map[string]*dcItem
}

type dcItem struct {
	size int64
	used time.Time
}

func openDiskCache(dir string, limit int64) (*diskCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &diskCache{dir: dir, limit: limit, items: map[string]*dcItem{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.Contains(name, ".partial-") {
			os.Remove(p) // left over from a crash
			return nil
		}
		if !isHex(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		c.items[name] = &dcItem{size: info.Size(), used: info.ModTime()}
		c.total += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.evict("")
	c.mu.Unlock()
	return c, nil
}

func (c *diskCache) path(oid string) string {
	return filepath.Join(c.dir, oid[:2], oid)
}

func (c *diskCache) lookup(oid string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[oid]
	if !ok {
		return "", false
	}
	now := time.Now()
	it.used = now
	p := c.path(oid)
	os.Chtimes(p, now, now) // persist LRU order across restarts
	return p, true
}

func (c *diskCache) create(oid string) (*os.File, error) {
	d := filepath.Join(c.dir, oid[:2])
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(d, oid+".partial-*")
}

func (c *diskCache) commit(oid, tmp string, size int64) error {
	if err := os.Rename(tmp, c.path(oid)); err != nil {
		os.Remove(tmp)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[oid]; !ok {
		c.items[oid] = &dcItem{size: size, used: time.Now()}
		c.total += size
	}
	c.evict(oid)
	return nil
}

// evict must be called with c.mu held.
func (c *diskCache) evict(keep string) {
	for c.total > c.limit {
		var victim string
		var oldest time.Time
		for oid, it := range c.items {
			if oid != keep && (victim == "" || it.used.Before(oldest)) {
				victim, oldest = oid, it.used
			}
		}
		if victim == "" {
			return
		}
		os.Remove(c.path(victim)) // open readers keep their fd
		c.total -= c.items[victim].size
		delete(c.items, victim)
	}
}
