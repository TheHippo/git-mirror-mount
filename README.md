# git-mirror-mount

Mounts one branch of a bare / `--mirror` git repository as a read-only FUSE
file system, so you can use `ls`, `grep`, editors, etc. without checking out.

```
git-mirror-mount [flags] <repo> <branch> <mountpoint>
```

## Build

```sh
go mod tidy        # fills in go.sum for golang.org/x/sys
go build -o git-mirror-mount .
```

Requirements: Linux with FUSE (`fusermount3`/`fusermount`), git >= 2.36.

## Usage

```sh
git clone --mirror https://example.com/huge.git ~/mirrors/huge.git
mkdir -p ~/huge
git-mirror-mount -cache-dir ~/.cache/git-mirror-mount ~/mirrors/huge.git main ~/huge &
# ... later
fusermount3 -u ~/huge     # or: kill the process (SIGINT/SIGTERM)
```

`<branch>` can be anything git can resolve: `main`, `refs/heads/release`, a
tag, or a commit hash (which then never changes).

### Running as a systemd user service (recommended)

`contrib/` contains a unit template for the mount (one instance per mirror)
and an optional daily fetch timer that runs `git fetch --prune` and then
reloads the mount:

```sh
cp contrib/git-mirror-*@.* ~/.config/systemd/user/
mkdir -p ~/.config/git-mirror-mount
cp contrib/example.env ~/.config/git-mirror-mount/huge.env   # edit paths
systemctl --user daemon-reload
systemctl --user enable --now git-mirror-mount@huge.service
systemctl --user enable --now git-mirror-fetch@huge.timer    # optional
```

The unit expects the binary at `~/go/bin/git-mirror-mount` (`go install .`).
Edit `ExecStart` if it lives elsewhere. Logs: `journalctl --user -u git-mirror-mount@huge`.

### AppArmor and snap terminals

Newer Ubuntu releases (and distros based on them) confine `fusermount3`
with an AppArmor profile. That profile refuses file descriptors inherited
from snap-confined processes, which shows up as:

```
fusermount3: file descriptor 3 is not a socket, can't send fuse fd
```

with `apparmor="DENIED" operation="file_inherit" ... peer="snap.<terminal>"`
in `journalctl -k`. This happens if your terminal is a snap (e.g. Alacritty):
every program started from it carries the snap's label.

* Started under a `snap.*` label, `git-mirror-mount` restarts itself with
  `systemd-run --user` automatically, attached to your terminal (`--pty`,
  so Ctrl-C works). `-no-reexec` turns that off.
* As a systemd user service (above) it never runs under a snap label.
* For any other mount failure under a confined label, it prints this
  explanation instead of just fusermount3's exit status.

Closing the terminal of an automatically restarted instance sends it
SIGHUP, which means "refresh", not "quit". Stop it with Ctrl-C,
`fusermount3 -u <mountpoint>`, or `systemctl --user stop <unit>`.

### Daily updates

The mount follows the branch. After `git fetch`, the new commit shows up:

* automatically within `-refresh` (default 1m; `-refresh 0` disables polling), or
* right away when you send `SIGHUP`:

```sh
git -C ~/mirrors/huge.git fetch --prune && pkill -HUP -f 'git-mirror-mount .*huge.git'
```

### Layouts

**`-layout tree`** (default): the mount root is the branch's tree. On a
refresh, files that are already open keep their old content. A shell whose
working directory is inside the mount keeps seeing the old snapshot in that
directory until it `cd`s again via the root.

**`-layout snapshots`** makes that switch explicit and atomic:

```
mnt/current -> commits/<sha>      switches on refresh
mnt/commits/<sha>/...             every commit mounted during this session
mnt/commits/<any full commit id>  any other commit in the repo, on demand
```

Use `mnt/current/...` for "latest". A path below `commits/<sha>` stays valid
and unchanged for as long as you use it.

### mtimes

mtimes are deterministic and say when a path last changed **while this
mount was running**:

* At mount time, every path gets the commit time of the mounted commit.
* On each refresh, the paths `git diff-tree` reports (changed files and
  every directory containing them) get the new commit's time. Everything
  else keeps its time.

So `make`, `rsync -t` or `find -newer` see exactly the files that changed
since the mount started. The history is held in memory only, so it starts
over after a remount. Commits looked up via `commits/<sha>` that are not
part of the mounted timeline use their own commit time for every path.

### Large files

Nothing is read on `open()`. On the first `read()`:

* Blobs up to `-max-inmem-mb` are loaded whole into an in-memory LRU (`-cache-mb`).
* Larger blobs are **streamed**: `git cat-file` writes into a spool file in
  the background, and each read only waits for its byte range. Sequential
  readers (`cat`, `grep`, `cp`, …) get data at once. Concurrent opens of the
  same blob share one stream.
* With `-cache-dir`, spool files are kept, keyed by object id, with
  `-cache-dir-mb` as the limit and LRU eviction. A later open, also after a
  remount, is served from disk. After each refresh, changed large blobs are
  **prefetched** into the cache in the background.

Random access near the end of a large blob still has to wait until git has
inflated everything before it, because git objects are compressed streams
and often deltas. Only the first access pays that cost; with `-cache-dir`
it's paid once per blob.

### Git LFS

With `-lfs`, blobs that are LFS pointers to an object present in
`<repo>/lfs/objects` are shown with that object's size and content. Fill
the store with:

```sh
git -C ~/mirrors/huge.git lfs fetch --all   # or: --recent / -I <paths>
```

Pointers whose object is missing locally are shown as the pointer file.
Detecting pointers costs one extra pipelined read per directory for blobs
between 100 and 1024 bytes.

### Protecting snapshots from `git gc`

A branch that moves forward keeps old commits reachable, so this only
matters for force-pushed or deleted branches. With `-pin-ref refs/mount/main`
the tool keeps `refs/mount/main/current` and `refs/mount/main/previous`
pointing at the mounted commits, so `gc` can't prune their objects.

A `--mirror` fetch uses `+refs/*:refs/*` with pruning, so every
`fetch --prune` deletes these refs. The tool re-creates them on its next
refresh check (every `-refresh` interval, or `SIGHUP`). In that short window
the objects are only protected by gc's grace period (`gc.pruneExpire`,
default 2 weeks). To avoid the gap completely, narrow the mirror's refspec,
e.g. `git config remote.origin.fetch '+refs/heads/*:refs/heads/*'`.

## Flags

| flag | default | meaning |
|---|---|---|
| `-layout` | `tree` | `tree` or `snapshots` (see above) |
| `-refresh` | `1m` | how often to check whether the branch moved (`0` = only on SIGHUP) |
| `-cache-dir` | — | persistent on-disk cache for large blobs; enables prefetch |
| `-cache-dir-mb` | `10240` | size limit of `-cache-dir` |
| `-lfs` | off | resolve Git LFS pointers from `<repo>/lfs/objects` |
| `-pin-ref` | — | ref prefix for pinning mounted commits, e.g. `refs/mount/main` |
| `-procs` | `4` | number of parallel `git cat-file` processes |
| `-cache-mb` | `256` | in-memory LRU cache for blob contents |
| `-max-inmem-mb` | `32` | larger blobs are streamed via a spool file |
| `-no-reexec` | off | don't restart via `systemd-run --user` under a snap AppArmor label |
| `-allow-other` | off | allow other users (needs `user_allow_other` in `/etc/fuse.conf`) |
| `-debug` | off | log every FUSE request |

## How it works

* Objects are read through long-running `git cat-file --batch-command`
  processes (a small pool), so it uses git's own pack and index handling,
  including SHA-256 repos and multi-pack-index. There is no extra index or
  database.
* Trees are loaded lazily, per directory, the first time it is looked up. The
  sizes of all blobs in a tree come from one pipelined `info` batch.
* Every inode below the root is immutable: one tree or blob, at one
  position, with one mtime. On refresh only the root (or `current`)
  changes, and it is invalidated in the kernel with `NotifyEntry`. That
  keeps kernel caching aggressive (1h timeouts, `FOPEN_KEEP_CACHE`) and still
  correct. Unchanged files keep their inode, so their page cache survives a
  refresh.
* Mapping: `100644` → `0444`, `100755` → `0555`, `120000` → symlink,
  `160000` (submodule) → empty directory.

## Not supported, on purpose

* **`.gitattributes` filters and line-ending conversion** (`eol`, `text`,
  smudge filters): the mount shows the blobs as stored. Applying filters
  would make content depend on configuration, and it breaks the
  "size = blob size" shortcut that keeps `ls -l` cheap.
* **A `.git` inside the mount:** git would treat the mount as a working
  tree with no index, and `git status` would hash every file. Run git
  commands against the mirror instead (`git -C <repo> log -- <path>`).
* **Remembering mtimes across remounts:** see "mtimes" above.
