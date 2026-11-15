package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		[]string{"PATH=/usr/bin", "SECRET=x", "GIT_TRACE=1", "TMPDIR=/t"})
	s := strings.Join(got, " ")
	for _, want := range []string{"--user", "--pty", "--same-dir", "--setenv=" + reexecEnv + "=1",
		"--setenv=PATH=/usr/bin", "--setenv=GIT_TRACE=1", "--setenv=TMPDIR=/t", "-- /bin/gmm -lfs repo main mnt"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
	if strings.Contains(s, "SECRET") {
		t.Errorf("leaked unrelated env: %q", s)
	}
	if s2 := strings.Join(reexecArgs("/x", nil, false, nil), " "); !strings.Contains(s2, "--pipe --wait") {
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
			maybeReexec(true)
			os.WriteFile(os.Getenv("GMM_OUT"), []byte(strings.Join(os.Args[1:], " ")), 0o644)
			os.Exit(7)
		}
		maybeReexec(true) // exits with the child's code
		os.Exit(99)       // not reached
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
	if !strings.Contains(string(b), "restarting via systemd-run") {
		t.Errorf("expected note on stderr, got %s", b)
	}
}
