# AGENTS.md

Guidance for coding agents working on `git-mirror-mount`: a read-only FUSE
file system (Go, `github.com/hanwen/go-fuse/v2`) that exposes one branch of a
bare / `--mirror` git repository. User-facing docs are in `README.md`.
Read `DECISIONS.md` before changing the architecture or reopening a
rejected option: it records why things are the way they are.

## Commands

```sh
go mod tidy                      # go.sum may be incomplete (golang.org/x/sys)
go build -o git-mirror-mount .
go vet ./...
go test ./...                    # unit tests; needs git >= 2.36, no FUSE
gofmt -l .                       # must print nothing
```

Run all of these before you call a change done.

## Layout

| file | contents |
|---|---|
| `main.go` | flags, mount options, layout selection, signal and refresh loop |
| `fs.go` | `gitFS.refresh` (snapshots, mtimes, pinning, prefetch), inode numbering, FUSE nodes for both layouts, file handles, blob LRU |
| `spool.go` | streaming of large blobs (`spool`, `spoolMgr`) and the on-disk cache (`diskCache`) |
| `git.go` | `git cat-file --batch-command` pool, tree parsing, LFS pointers, `diffTrees`, `resolve`, `ensureRef` |
| `confinement.go` | AppArmor label detection, automatic restart via `systemd-run --user`, mount-failure hints |
| `contrib/` | systemd user units (mount template, fetch service and timer) and `example.env` |
| `confinement_test.go` | label parsing, `systemd-run` arguments, restart end-to-end with a stand-in `systemd-run` |
| `git_test.go` | unit tests (tree parsing, pool, LFS pointers, diff-tree, disk cache) |

## Design invariants (do not break)

1. **Everything below the root(s) is immutable.** A `dirNode` stands for one
   tree object and a `fileNode` for one blob, each with one mtime. Nodes
   never change their content or attributes after they are created. Only
   `gitFS.cur` (an `atomic.Pointer[snapshot]`) is swapped, in `refresh()`.
   That moves the `treeRoot` contents or the `current` symlink of
   `snapRoot`. This is why 1h kernel timeouts and `FOPEN_KEEP_CACHE` are safe.
2. **Inode numbers are derived, not allocated.** `childIno` is the only
   place that computes them, and both `lookupIn` and `dirStream` must use it.
   - files: `inoHash("f", path, oid, mode, mtime)`, shared across snapshots while unchanged, so the page cache survives a refresh
   - symlinks: `inoHash("l", path, oid, mtime)`
   - dirs: `inoHash("d", parentIno, name, oid, mtime)`, unique per parent, because the kernel must never see a directory under two parents
   - commit roots: `inoHash("c", sha)`; `commits/`: `inoHash("commits")`; `current`: `inoHash("current", sha)`
   - 0, 1 (root) and `^0` are reserved
   - The mtime is part of the hash so that A→B→A content changes get a new inode with the new mtime.
3. **mtimes come from the snapshot, never from when a node was created.**
   `snapshot.times` holds per-path overrides of `snapshot.base`. Each
   refresh copies the old map and sets every path from `diffTrees` to the new
   commit time. Published snapshots are read-only. Node creation time or
   kernel cache eviction must never influence an mtime.
4. **Refresh only invalidates what is mutable.** Layout `tree`: the
   top-level names in the diff (`onSwitch` in `newTreeRoot`). Layout
   `snapshots`: `current`. Deeper paths hang off immutable inodes.
   `commitsDir.Lookup` returns ENOENT without a negative-cache timeout, so a
   commit id that doesn't exist yet can appear after the next fetch.
5. **Spools are reference-counted.** `spoolMgr.acquire` and `release` must
   stay balanced: each `fileHandle` holds one reference, and the filler
   goroutine holds one until the stream finishes. A failed spool is removed
   from `active` so the next open retries. With a disk cache the spool file is
   `<oid>.partial-*` until it is complete, then renamed. Never serve a
   partial file from the cache.
6. **Each `catFile` is used by one goroutine at a time.** Use it only through
   `pool.with`/`get`/`put`. After any protocol or I/O error, set `broken = true`
   so the pool discards the process instead of reusing a desynced stream.
   `pool.reset()` after a ref change makes sure new packs are seen.
7. **Pipelined commands must write from a goroutine** (see `infoMany`, `contentsMany`).
   Otherwise a full stdout pipe deadlocks against our writes.
8. Strictly read-only: `ro` mount option, and `Open` rejects write flags with
   `EROFS`. Don't add write support.

9. **Restarting must not loop.** `maybeReexec` runs before anything else
   in `main` and is guarded by `GIT_MIRROR_MOUNT_REEXEC`. Only an allow-list
   of environment variables is passed to `systemd-run` (PATH, TMPDIR, HOME,
   GIT_*). The units in `contrib/` use `-no-reexec`.

## Git protocol notes

- `--batch-command` needs git >= 2.36 (`checkGitVersion`). Responses:
  `<oid> <type> <size>\n<content>\n` or `<obj> missing\n`.
- Hash length comes from the resolved commit id (20 bytes for SHA-1, 32 for
  SHA-256). Never hard-code 20.
- Resolve refs with a fresh `git` process (`resolve`), not through the
  long-running cat-file, which can hold stale ref state.
- Changed paths come from `git diff-tree -r -t -z --no-renames` (`-t` adds
  the directories, which need new mtimes too).
- Mode mapping: `100644`→0444, `100755`→0555, `120000`→symlink,
  `160000` (gitlink)→empty dir. `parseTree` normalises legacy modes like
  `100664`.

## End-to-end testing (needs FUSE)

Needs `/dev/fuse` and `fusermount3` (or root). There is no automated
test for this; do it by hand when you touch `fs.go` or the refresh logic:

```sh
git init -q -b main src && (cd src && <create files, symlinks, exec bits, big file> && git add -A && git commit -qm one)
git clone -q --mirror src mirror.git && git clone -q src checkout && mkdir mnt
./git-mirror-mount -refresh 0 mirror.git main mnt & PID=$!
diff -r --no-dereference -x .git checkout mnt        # must be empty
# commit to src, then:
git -C mirror.git fetch -q --prune && kill -HUP $PID
# re-clone checkout and diff again; also check open fds still read old content
kill $PID                                            # must unmount cleanly
```

Also check, depending on what you touched:

- **mtimes:** unchanged paths keep their time across a refresh and after
  `echo 3 > /proc/sys/vm/drop_caches`. A file changed A→B→A gets the newest time.
- **streaming:** `head -c 100` on a cold blob of several 100 MB returns in
  milliseconds. Concurrent `cmp`s of the same blob all succeed.
- **`-cache-dir`:** after a remount, the blob is served from the cache. After a
  refresh, changed large blobs show up in the cache (prefetch).
- **`-pin-ref`:** the refs are re-created after `fetch --prune` deletes them.
- **`-lfs`:** put an object into `<repo>/lfs/objects/aa/bb/<sha256>` and commit
  a matching pointer file. `cmp` against the object must succeed.
- **`-layout snapshots`:** `current` switches on SIGHUP, and an old
  `commits/<sha>` keeps its old content.

For a realistic check, mirror-clone a big repo (e.g. golang/go) and compare
with `git archive <branch> | tar -x`.

## Conventions

- Plain standard library plus go-fuse. Ask before adding dependencies (no go-git, no libgit2/cgo).
- Log errors with `log.Printf` and return `syscall.EIO` to the kernel. Never panic in FUSE handlers.
- Keep the README flag table in sync with `main.go`.
