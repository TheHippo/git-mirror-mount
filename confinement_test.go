package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLabels(t *testing.T) {
	cases := []struct {
		in             string
		snap, confined bool
	}{
		{"unconfined", false, false},
		{"", false, false},
		{"snap.alacritty.alacritty (complain)", true, true},
		{"snap.alacritty.alacritty (enforce)", true, true},
		{"/usr/bin/foo (enforce)", false, true},
	}
	for _, c := range cases {
		if isSnapLabel(c.in) != c.snap || isConfined(c.in) != c.confined {
			t.Errorf("%q: snap=%v confined=%v", c.in, isSnapLabel(c.in), isConfined(c.in))
		}
	}
}

func TestReexecArgs(t *testing.T) {
	got := reexecArgs("/bin/gmm", []string{"-lfs", "repo", "main", "mnt"}, true,
		[]string{"PATH=/usr/bin", "SECRET=x", "GIT_TRACE=1", "TMPDIR=/t"}, "gmm-1", `/mnt/my "dir"`)
	s := strings.Join(got, " ")
	for _, want := range []string{"--user", "--pty", "--same-dir", "--setenv=" + reexecEnv + "=1",
		"--setenv=PATH=/usr/bin", "--setenv=GIT_TRACE=1", "--setenv=TMPDIR=/t", "-- /bin/gmm -lfs repo main mnt",
		"--unit=gmm-1", `/bin/sh -c "\"$$0\" -uz \"$$1\" </dev/null >/dev/null 2>&1"`, `"/mnt/my \"dir\""`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
	if strings.Contains(s, "SECRET") {
		t.Errorf("leaked unrelated env: %q", s)
	}
	if s2 := strings.Join(reexecArgs("/x", nil, false, nil, "u", ""), " "); !strings.Contains(s2, "--pipe --wait") {
		t.Errorf("non-interactive: %q", s2)
	}
}

// TestReexecEndToEnd runs the test binary itself under a fake snap label,
// with a stand-in systemd-run that just executes what follows "--" with the
// --setenv variables. The child must see the guard variable, not re-exec
// again, and get the original arguments.
func TestReexecEndToEnd(t *testing.T) {
	if os.Getenv("GMM_CHILD") != "" {
		apparmorLabel = func() string { return "snap.alacritty.alacritty (complain)" }
		if os.Getenv(reexecEnv) != "" {
			// Re-executed child: report args, no further re-exec.
			maybeReexec(true, "mnt")
			os.WriteFile(os.Getenv("GMM_OUT"), []byte(strings.Join(os.Args[1:], " ")), 0o644)
			os.Exit(7)
		}
		maybeReexec(true, "mnt") // exits with the child's code
		os.Exit(99)              // not reached
	}
	dir := t.TempDir()
	fake := `#!/bin/sh
while [ "$1" != "--" ]; do
  case "$1" in --setenv=*) export "${1#--setenv=}";; esac
  shift
done
shift
exec "$@"
`
	os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(fake), 0o755)
	out := filepath.Join(dir, "out")
	cmd := exec.Command(os.Args[0], "-test.run=^TestReexecEndToEnd$")
	cmd.Env = append(os.Environ(), "GMM_CHILD=1", "GMM_OUT="+out, "PATH="+dir+":"+os.Getenv("PATH"))
	b, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Fatalf("want exit 7 (propagated from re-executed child), got %v\n%s", err, b)
	}
	args, _ := os.ReadFile(out)
	if !strings.Contains(string(args), "-test.run=^TestReexecEndToEnd$") {
		t.Fatalf("child got args %q", args)
	}
	if !strings.Contains(string(b), "restarting as systemd user unit") {
		t.Errorf("expected note on stderr, got %s", b)
	}
}

func TestUnitQuote(t *testing.T) {
	if got, want := unitQuote(`a b%c$d"e\`), `"a b%%c$$d\"e\\"`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestWrapperForwardsSignals: while the restarted instance runs, the wrapper
// must survive SIGHUP, turn it (and SIGTERM) into a signal for the unit's main
// process, and exit with the server's status. Fake systemd-run/systemctl stand
// in for the real ones.
func TestWrapperForwardsSignals(t *testing.T) {
	if os.Getenv("GMM_WRAPPER") != "" {
		apparmorLabel = func() string { return "snap.alacritty.alacritty (complain)" }
		maybeReexec(true, "mnt") // exits with the "server"'s code
		os.Exit(99)
	}
	dir := t.TempDir()
	started, stop, calls := filepath.Join(dir, "started"), filepath.Join(dir, "stop"), filepath.Join(dir, "calls")
	// The "server" idles until the fake systemctl is asked to TERM it.
	os.WriteFile(filepath.Join(dir, "systemd-run"), []byte("#!/bin/sh\n"+
		"touch "+started+"\nwhile [ ! -e "+stop+" ]; do sleep 0.05; done\nexit 5\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"+
		"case \"$*\" in *SIGTERM*) touch "+stop+";; esac\n"), 0o755)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWrapperForwardsSignals$")
	cmd.Env = append(os.Environ(), "GMM_WRAPPER=1", "PATH="+dir+":"+os.Getenv("PATH"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor := func(path string) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", path)
	}
	waitFor(started)
	time.Sleep(200 * time.Millisecond) // let the wrapper install its handlers
	cmd.Process.Signal(syscall.SIGHUP)
	waitFor(calls)
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("wrapper died of SIGHUP: %v", err)
	}
	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 5 {
		t.Fatalf("want exit 5 (the server's status), got %v", err)
	}
	b, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 ||
		!strings.Contains(lines[0], "--user kill --kill-whom=main --signal=SIGHUP git-mirror-mount-") ||
		!strings.Contains(lines[1], "--signal=SIGTERM") {
		t.Errorf("unexpected systemctl calls: %q", lines)
	}
}
