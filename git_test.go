package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTree(t *testing.T) {
	h := make([]byte, 20)
	h[19] = 0xab
	data := append([]byte("100664 a.txt\x00"), h...)
	data = append(data, append([]byte("40000 dir\x00"), h...)...)
	got, err := parseTree(data, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].name != "a.txt" || got[0].mode != modeBlob ||
		got[1].mode != modeTree || got[1].oid != "00000000000000000000000000000000000000ab" {
		t.Fatalf("unexpected: %+v", got)
	}
	if _, err := parseTree(data[:len(data)-1], 20); err == nil {
		t.Fatal("expected error for truncated tree")
	}
}

func TestPoolAgainstRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := checkGitVersion(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "f.txt"), []byte("hello\n"), 0o644)
	run("add", "-A")
	run("commit", "-qm", "x")

	info, err := resolve(filepath.Join(dir, ".git"), "main")
	if err != nil {
		t.Fatal(err)
	}
	p := newPool(filepath.Join(dir, ".git"), 2)
	defer p.closeAll()
	root, err := p.readTree(info.tree, len(info.commit)/2, "")
	if err != nil || len(root) != 1 || root[0].name != "d" {
		t.Fatalf("root: %+v %v", root, err)
	}
	sub, err := p.readTree(root[0].oid, len(info.commit)/2, "")
	if err != nil || len(sub) != 1 || sub[0].size != 6 {
		t.Fatalf("sub: %+v %v", sub, err)
	}
	b, err := p.readObject(sub[0].oid, "blob")
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("blob: %q %v", b, err)
	}
	if _, err := p.readObject("0000000000000000000000000000000000000001", ""); err != errMissing {
		t.Fatalf("expected errMissing, got %v", err)
	}
}

func TestParseLFSPointer(t *testing.T) {
	oid := "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	ptr := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 12345\n"
	got, size, ok := parseLFSPointer([]byte(ptr))
	if !ok || got != oid || size != 12345 {
		t.Fatalf("got %q %d %v", got, size, ok)
	}
	for _, bad := range []string{
		"hello world",
		"version https://git-lfs.github.com/spec/v1\noid sha256:xyz\nsize 1\n",
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\n",
	} {
		if _, _, ok := parseLFSPointer([]byte(bad)); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDiffTrees(t *testing.T) {
	if err := checkGitVersion(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "x"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep"), []byte("k"), 0o644)
	git("add", "-A")
	git("commit", "-qm", "1")
	c1, _ := resolve(filepath.Join(dir, ".git"), "main")
	os.WriteFile(filepath.Join(dir, "a", "b", "x"), []byte("2"), 0o644)
	git("commit", "-qam", "2")
	c2, _ := resolve(filepath.Join(dir, ".git"), "main")

	d, err := diffTrees(filepath.Join(dir, ".git"), c1.tree, c2.tree)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range d {
		paths[e.path] = true
	}
	if len(paths) != 3 || !paths["a"] || !paths["a/b"] || !paths["a/b/x"] {
		t.Fatalf("unexpected diff: %+v", d)
	}
}

func TestDiskCacheEviction(t *testing.T) {
	dir := t.TempDir()
	c, err := openDiskCache(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	put := func(oid string, n int) {
		f, err := c.create(oid)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(make([]byte, n))
		f.Close()
		if err := c.commit(oid, f.Name(), int64(n)); err != nil {
			t.Fatal(err)
		}
	}
	put("aa01", 6)
	put("bb02", 6) // exceeds limit: aa01 evicted
	if _, ok := c.lookup("aa01"); ok {
		t.Fatal("aa01 should be evicted")
	}
	if p, ok := c.lookup("bb02"); !ok {
		t.Fatal("bb02 missing")
	} else if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	// Reopen: state is rebuilt from disk.
	c2, _ := openDiskCache(dir, 10)
	if _, ok := c2.lookup("bb02"); !ok || c2.total != 6 {
		t.Fatalf("reopen: total=%d", c2.total)
	}
}

func TestGitCommandOwnProcessGroup(t *testing.T) {
	cmd := gitCommand("version")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("git children must run in their own process group, or a terminal hangup kills them")
	}
	if out, err := cmd.Output(); err != nil || !strings.HasPrefix(string(out), "git version") {
		t.Fatalf("%q %v", out, err)
	}
}
