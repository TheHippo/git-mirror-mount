package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// gitCommand builds a git command that runs in its own process group. The
// server must keep working when its controlling terminal hangs up or someone
// signals "the process group" (e.g. `pkill -HUP -f`), and git children left in
// the server's group would die of that SIGHUP.
func gitCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// errMissing is returned when an object does not exist in the repository.
var errMissing = errors.New("object missing")

// Git tree entry modes.
const (
	modeTree    = 0o040000
	modeBlob    = 0o100644
	modeExec    = 0o100755
	modeSymlink = 0o120000
	modeGitlink = 0o160000
)

type treeEntry struct {
	name string
	mode uint32
	oid  string // hex
	size int64  // blob size (or LFS object size); -1 if not a blob
	lfs  string // path of the local LFS object if this blob is an LFS pointer
}

// catFile is one long-running `git cat-file --batch-command` process.
// Not safe for concurrent use; the pool hands out exclusive access.
type catFile struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	in     *bufio.Writer
	out    *bufio.Reader
	gen    int
	broken bool
}

func startCatFile(gitDir string, gen int) (*catFile, error) {
	cmd := gitCommand("--git-dir="+gitDir, "cat-file", "--batch-command")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting git cat-file: %w", err)
	}
	return &catFile{
		cmd:   cmd,
		stdin: stdin,
		in:    bufio.NewWriter(stdin),
		out:   bufio.NewReaderSize(stdout, 1<<16),
		gen:   gen,
	}, nil
}

func (c *catFile) close() {
	c.stdin.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

// header reads "<oid> <type> <size>\n" or "<obj> missing\n".
func (c *catFile) header() (typ string, size int64, err error) {
	line, err := c.out.ReadString('\n')
	if err != nil {
		c.broken = true
		return "", 0, fmt.Errorf("reading cat-file header: %w", err)
	}
	f := strings.Fields(line)
	if len(f) == 2 && (f[1] == "missing" || f[1] == "ambiguous") {
		return "", 0, errMissing
	}
	if len(f) != 3 {
		c.broken = true
		return "", 0, fmt.Errorf("unexpected cat-file header %q", line)
	}
	size, err = strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		c.broken = true
		return "", 0, fmt.Errorf("bad size in cat-file header %q", line)
	}
	return f[1], size, nil
}

func (c *catFile) send(cmds ...string) error {
	for _, s := range cmds {
		if _, err := c.in.WriteString(s); err != nil {
			c.broken = true
			return err
		}
	}
	if err := c.in.Flush(); err != nil {
		c.broken = true
		return err
	}
	return nil
}

// contents streams an object's content into w.
func (c *catFile) contents(oid string, w io.Writer) (typ string, size int64, err error) {
	if err := c.send("contents ", oid, "\n"); err != nil {
		return "", 0, err
	}
	typ, size, err = c.header()
	if err != nil {
		return "", 0, err
	}
	if _, err := io.CopyN(w, c.out, size); err != nil {
		c.broken = true
		return "", 0, fmt.Errorf("reading object %s: %w", oid, err)
	}
	if b, err := c.out.ReadByte(); err != nil || b != '\n' {
		c.broken = true
		return "", 0, fmt.Errorf("missing trailer after object %s", oid)
	}
	return typ, size, nil
}

// infoMany returns the sizes of many objects with a single pipelined
// round-trip. Writing happens in a goroutine so a full stdout pipe cannot
// deadlock us.
func (c *catFile) infoMany(oids []string) ([]int64, error) {
	werr := make(chan error, 1)
	go func() {
		for _, o := range oids {
			if _, err := c.in.WriteString("info " + o + "\n"); err != nil {
				werr <- err
				return
			}
		}
		werr <- c.in.Flush()
	}()
	sizes := make([]int64, len(oids))
	var firstErr error
	for i := range oids {
		_, size, err := c.header()
		if err != nil {
			if err == errMissing {
				sizes[i] = 0
				continue
			}
			firstErr = err
			break
		}
		sizes[i] = size
	}
	if err := <-werr; err != nil && firstErr == nil {
		c.broken = true
		firstErr = err
	}
	return sizes, firstErr
}

// contentsMany fetches the contents of many (small) objects in one pipelined
// round-trip.
func (c *catFile) contentsMany(oids []string) ([][]byte, error) {
	werr := make(chan error, 1)
	go func() {
		for _, o := range oids {
			if _, err := c.in.WriteString("contents " + o + "\n"); err != nil {
				werr <- err
				return
			}
		}
		werr <- c.in.Flush()
	}()
	out := make([][]byte, len(oids))
	var firstErr error
	for i := range oids {
		_, size, err := c.header()
		if err == errMissing {
			continue
		}
		if err != nil {
			firstErr = err
			break
		}
		b := make([]byte, size+1) // + trailing newline
		if _, err := io.ReadFull(c.out, b); err != nil || b[size] != '\n' {
			c.broken = true
			firstErr = fmt.Errorf("reading object %s: short read", oids[i])
			break
		}
		out[i] = b[:size]
	}
	if err := <-werr; err != nil && firstErr == nil {
		c.broken = true
		firstErr = err
	}
	return out, firstErr
}

// pool hands out cat-file processes so that several FUSE requests can be
// served concurrently (one slow multi-GB blob does not block everything).
type pool struct {
	gitDir string
	sem    chan struct{}
	mu     sync.Mutex
	idle   []*catFile
	gen    int
}

func newPool(gitDir string, size int) *pool {
	return &pool{gitDir: gitDir, sem: make(chan struct{}, size)}
}

func (p *pool) get() (*catFile, error) {
	p.sem <- struct{}{}
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, nil
	}
	gen := p.gen
	p.mu.Unlock()
	c, err := startCatFile(p.gitDir, gen)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return c, nil
}

func (p *pool) put(c *catFile) {
	p.mu.Lock()
	if c.broken || c.gen != p.gen {
		p.mu.Unlock()
		c.close()
	} else {
		p.idle = append(p.idle, c)
		p.mu.Unlock()
	}
	<-p.sem
}

// reset retires all processes. Used after the branch moved: a fresh process
// is guaranteed to see newly fetched packs and not hold deleted ones open.
func (p *pool) reset() {
	p.mu.Lock()
	p.gen++
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, c := range idle {
		c.close()
	}
}

func (p *pool) closeAll() { p.reset() }

func (p *pool) with(f func(c *catFile) error) error {
	c, err := p.get()
	if err != nil {
		return err
	}
	defer p.put(c)
	return f(c)
}

func (p *pool) readObject(oid string, wantType string) ([]byte, error) {
	var buf bytes.Buffer
	err := p.with(func(c *catFile) error {
		typ, _, err := c.contents(oid, &buf)
		if err != nil {
			return err
		}
		if wantType != "" && typ != wantType {
			return fmt.Errorf("object %s is a %s, expected %s", oid, typ, wantType)
		}
		return nil
	})
	return buf.Bytes(), err
}

// readTree loads and parses a tree, including the sizes of all blobs in it.
// If lfsDir is set (…/lfs/objects), blobs that are Git LFS pointers to an
// object present in lfsDir are resolved to that object.
func (p *pool) readTree(oid string, hashLen int, lfsDir string) ([]treeEntry, error) {
	var entries []treeEntry
	err := p.with(func(c *catFile) error {
		var buf bytes.Buffer
		typ, _, err := c.contents(oid, &buf)
		if err != nil {
			return err
		}
		if typ != "tree" {
			return fmt.Errorf("object %s is a %s, expected tree", oid, typ)
		}
		entries, err = parseTree(buf.Bytes(), hashLen)
		if err != nil {
			return err
		}
		var blobs []string
		var idx []int
		for i := range entries {
			switch entries[i].mode {
			case modeBlob, modeExec, modeSymlink:
				blobs = append(blobs, entries[i].oid)
				idx = append(idx, i)
			}
		}
		if len(blobs) == 0 {
			return nil
		}
		sizes, err := c.infoMany(blobs)
		if err != nil {
			return err
		}
		for j, i := range idx {
			entries[i].size = sizes[j]
		}
		if lfsDir == "" {
			return nil
		}
		// Pointer files are small; only look at blobs in that size range.
		var cand []string
		idx = idx[:0]
		for i := range entries {
			e := &entries[i]
			if (e.mode == modeBlob || e.mode == modeExec) && e.size >= lfsMinPointer && e.size <= lfsMaxPointer {
				cand = append(cand, e.oid)
				idx = append(idx, i)
			}
		}
		if len(cand) == 0 {
			return nil
		}
		bodies, err := c.contentsMany(cand)
		if err != nil {
			return err
		}
		for j, i := range idx {
			lfsOid, size, ok := parseLFSPointer(bodies[j])
			if !ok {
				continue
			}
			path := filepath.Join(lfsDir, lfsOid[0:2], lfsOid[2:4], lfsOid)
			if st, err := os.Stat(path); err == nil && st.Size() == size {
				entries[i].lfs = path
				entries[i].size = size
			}
		}
		return nil
	})
	return entries, err
}

const (
	lfsMinPointer = 100  // a real pointer is ~130 bytes
	lfsMaxPointer = 1024 // spec limit
)

var lfsPrefix = []byte("version https://git-lfs.github.com/spec/v1\n")

// parseLFSPointer recognises a Git LFS pointer file.
func parseLFSPointer(b []byte) (oid string, size int64, ok bool) {
	if !bytes.HasPrefix(b, lfsPrefix) {
		return "", 0, false
	}
	size = -1
	for _, line := range strings.Split(string(b[len(lfsPrefix):]), "\n") {
		switch {
		case strings.HasPrefix(line, "oid sha256:"):
			oid = strings.TrimPrefix(line, "oid sha256:")
		case strings.HasPrefix(line, "size "):
			size, _ = strconv.ParseInt(strings.TrimPrefix(line, "size "), 10, 64)
		}
	}
	if len(oid) != 64 || !isHex(oid) || size < 0 {
		return "", 0, false
	}
	return oid, size, true
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

type diffEntry struct {
	path    string
	newMode uint32 // 0 if deleted
	newOid  string
}

// diffTrees lists every path (blobs and trees) that differs between two
// trees, recursively.
func diffTrees(gitDir, oldTree, newTree string) ([]diffEntry, error) {
	cmd := gitCommand("--git-dir="+gitDir, "diff-tree", "-r", "-t", "-z",
		"--no-renames", "--no-commit-id", oldTree, newTree)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("diff-tree: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	// Records: ":<oldmode> <newmode> <oldoid> <newoid> <status>\0<path>\0"
	parts := strings.Split(string(out), "\x00")
	var res []diffEntry
	for i := 0; i+1 < len(parts); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(parts[i], ":"))
		if len(meta) < 5 {
			return nil, fmt.Errorf("diff-tree: unexpected record %q", parts[i])
		}
		mode, _ := strconv.ParseUint(meta[1], 8, 32)
		res = append(res, diffEntry{path: parts[i+1], newMode: uint32(mode), newOid: meta[3]})
	}
	return res, nil
}

// ensureRef points ref at oid unless it already does. Used to pin mounted
// commits so gc cannot prune their objects.
func ensureRef(gitDir, ref, oid string) error {
	out, err := gitCommand("--git-dir="+gitDir, "rev-parse", "--verify", "-q", ref).Output()
	if err == nil && strings.TrimSpace(string(out)) == oid {
		return nil
	}
	cmd := gitCommand("--git-dir="+gitDir, "update-ref", "-m", "git-mirror-mount pin", ref, oid)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("update-ref %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parseTree parses the binary tree format: "<mode> <name>\0<raw hash>"...
func parseTree(data []byte, hashLen int) ([]treeEntry, error) {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errors.New("corrupt tree: no mode separator")
		}
		mode, err := strconv.ParseUint(string(data[:sp]), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("corrupt tree: bad mode %q", data[:sp])
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+1+hashLen {
			return nil, errors.New("corrupt tree: truncated entry")
		}
		name := string(data[:nul])
		oid := fmt.Sprintf("%x", data[nul+1:nul+1+hashLen])
		data = data[nul+1+hashLen:]
		m := uint32(mode)
		// Normalise legacy modes (e.g. 100664) like git does.
		if m&0o170000 == 0o100000 {
			if m&0o111 != 0 {
				m = modeExec
			} else {
				m = modeBlob
			}
		}
		out = append(out, treeEntry{name: name, mode: m, oid: oid, size: -1})
	}
	return out, nil
}

type commitInfo struct {
	commit string
	tree   string
	time   time.Time
}

// resolve turns a branch/ref/commit name into commit, root tree and commit
// time. Uses a fresh git process so ref updates by `git fetch` are seen.
func resolve(gitDir, rev string) (commitInfo, error) {
	cmd := gitCommand("--git-dir="+gitDir, "show", "-s",
		"--format=%H %T %ct", rev+"^{commit}", "--")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return commitInfo{}, fmt.Errorf("resolving %q: %v: %s", rev, err, strings.TrimSpace(stderr.String()))
	}
	f := strings.Fields(string(out))
	if len(f) != 3 {
		return commitInfo{}, fmt.Errorf("resolving %q: unexpected output %q", rev, out)
	}
	ts, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return commitInfo{}, err
	}
	return commitInfo{commit: f[0], tree: f[1], time: time.Unix(ts, 0)}, nil
}

func checkGitVersion() error {
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		return fmt.Errorf("git not found: %w", err)
	}
	// "git version 2.43.0" (possibly with suffixes like ".windows.1")
	f := strings.Fields(string(out))
	if len(f) < 3 {
		return nil
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return nil
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	if major < 2 || (major == 2 && minor < 36) {
		return fmt.Errorf("git %s is too old; need >= 2.36 for cat-file --batch-command", f[2])
	}
	return nil
}
