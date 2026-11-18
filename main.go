// git-mirror-mount mounts the tree of a branch in a (bare / --mirror) git
// repository as a read-only FUSE file system.
//
//	git-mirror-mount [flags] <repo> <branch> <mountpoint>
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	log.SetFlags(log.LstdFlags)
	var (
		refresh    = flag.Duration("refresh", time.Minute, "how often to check whether the branch moved (0 = only on SIGHUP)")
		procs      = flag.Int("procs", 4, "number of parallel git cat-file processes")
		cacheMB    = flag.Int64("cache-mb", 256, "in-memory blob cache size in MiB")
		maxInMemMB = flag.Int64("max-inmem-mb", 32, "blobs larger than this (MiB) are spooled to a temp file on open")
		layout     = flag.String("layout", "tree", `"tree": mount root is the branch's tree; "snapshots": current -> commits/<sha>/`)
		cacheDir   = flag.String("cache-dir", "", "persistent on-disk cache for large blobs (enables prefetch after refresh)")
		cacheDirMB = flag.Int64("cache-dir-mb", 10240, "size limit of -cache-dir in MiB")
		lfs        = flag.Bool("lfs", false, "serve Git LFS objects present in <repo>/lfs/objects instead of pointer files")
		pinRef     = flag.String("pin-ref", "", "keep <prefix>/current and <prefix>/previous refs on the mounted commits so gc can't prune them (e.g. refs/mount/main)")
		noReexec   = flag.Bool("no-reexec", false, "don't restart via systemd-run --user when started under a snap AppArmor label")
		allowOther = flag.Bool("allow-other", false, "allow other users to access the mount (needs user_allow_other in /etc/fuse.conf)")
		debug      = flag.Bool("debug", false, "log every FUSE request")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <repo> <branch> <mountpoint>\n\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "Mounts <branch> of the git repository <repo> read-only at <mountpoint>.\n")
		fmt.Fprintf(os.Stderr, "Send SIGHUP to re-check the branch immediately (e.g. after git fetch).\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 3 {
		flag.Usage()
		os.Exit(2)
	}
	repo, rev, mnt := flag.Arg(0), flag.Arg(1), flag.Arg(2)

	// Must happen before anything else: a snap-confined process can't mount.
	maybeReexec(!*noReexec, mnt)

	// SIGHUP means "refresh" and must never kill us, not even while we are
	// still starting up (a terminal hangup or `pkill -HUP -f` can arrive at
	// any time). One queued SIGHUP becomes one refresh once the loop runs.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)
	// A vanished terminal or pipe must not kill us through a log write either:
	// with SIGPIPE caught, writes to a broken stdout/stderr just fail.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	if err := checkMountpoint(mnt); err != nil {
		log.Fatal(err)
	}
	if err := checkGitVersion(); err != nil {
		log.Fatal(err)
	}
	gitDir, err := findGitDir(repo)
	if err != nil {
		log.Fatal(err)
	}

	info, err := resolve(gitDir, rev)
	if err != nil {
		log.Fatal(err)
	}

	if *pinRef != "" && !strings.HasPrefix(*pinRef, "refs/") {
		log.Fatalf("-pin-ref must start with refs/")
	}

	p := newPool(gitDir, *procs)
	defer p.closeAll()
	var dc *diskCache
	if *cacheDir != "" {
		if dc, err = openDiskCache(*cacheDir, *cacheDirMB<<20); err != nil {
			log.Fatalf("cache dir: %v", err)
		}
	}
	g := &gitFS{
		pool:     p,
		gitDir:   gitDir,
		rev:      rev,
		hashLen:  len(info.commit) / 2, // 20 for SHA-1, 32 for SHA-256
		blobs:    newBlobCache(*cacheMB << 20),
		maxInMem: *maxInMemMB << 20,
		spools:   newSpoolMgr(p, dc, os.TempDir()),
		pinRef:   strings.TrimSuffix(*pinRef, "/"),
		seen:     map[string]*snapshot{},
	}
	if *lfs {
		g.lfsDir = lfsObjectsDir(gitDir)
	}
	var root fs.InodeEmbedder
	switch *layout {
	case "tree":
		root = newTreeRoot(g)
	case "snapshots":
		root = newSnapRoot(g)
	default:
		log.Fatalf("unknown -layout %q", *layout)
	}
	if err := g.refresh(); err != nil {
		log.Fatal(err)
	}

	// Everything except the root is immutable, so long timeouts are safe;
	// root entries are invalidated explicitly on refresh.
	long := time.Hour
	opts := &fs.Options{
		EntryTimeout:    &long,
		AttrTimeout:     &long,
		NegativeTimeout: &long,
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		MountOptions: fuse.MountOptions{
			FsName:     "gitmirror:" + gitDir,
			Name:       "git-mirror-mount",
			AllowOther: *allowOther,
			Options:    []string{"ro", "default_permissions"},
			Debug:      *debug,
		},
	}
	server, err := fs.Mount(mnt, root, opts)
	if err != nil {
		log.Fatalf("mount: %v%s", err, mountErrorHint(err))
	}
	log.Printf("mounted %s (%s @ %.12s) at %s", gitDir, rev, info.commit, mnt)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	var tick <-chan time.Time
	if *refresh > 0 {
		t := time.NewTicker(*refresh)
		defer t.Stop()
		tick = t.C
	}
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()

	for {
		select {
		case <-tick:
			if err := g.refresh(); err != nil {
				log.Printf("refresh: %v", err)
			}
		case s := <-sigs:
			if s == syscall.SIGHUP {
				if err := g.refresh(); err != nil {
					log.Printf("refresh: %v", err)
				}
				continue
			}
			log.Printf("%v: unmounting", s)
			if err := server.Unmount(); err != nil {
				log.Printf("unmount: %v (is the mount busy?)", err)
				continue
			}
		case <-done:
			return
		}
	}
}

// checkMountpoint turns the common failure cases into clear messages instead
// of fusermount3's exit status.
func checkMountpoint(mnt string) error {
	st, err := os.Stat(mnt)
	switch {
	case errors.Is(err, syscall.ENOTCONN):
		return fmt.Errorf("%s is a stale FUSE mount (a previous instance died); clean up with: fusermount3 -uz %s", mnt, mnt)
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("mountpoint %s does not exist; create it with: mkdir -p %s", mnt, mnt)
	case err != nil:
		return fmt.Errorf("mountpoint %s: %v", mnt, err)
	case !st.IsDir():
		return fmt.Errorf("mountpoint %s is not a directory", mnt)
	}
	return nil
}

// findGitDir accepts a bare/mirror repo or a normal working copy.
func findGitDir(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not a git repository", repo)
	}
	return strings.TrimSpace(string(out)), nil
}
