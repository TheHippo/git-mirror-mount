package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// AppArmor and snap confinement
//
// Newer Ubuntu releases confine /usr/bin/fusermount3 with an AppArmor
// profile. That profile denies file descriptors inherited from a
// snap-labelled process ("file_inherit ... peer=snap.<name>"), including
// the socket fusermount3 uses to hand back /dev/fuse. That's why mounting
// fails when git-mirror-mount is started from a terminal installed as a snap
// (e.g. Alacritty): every child of that terminal carries the snap's label,
// and a confined process cannot drop it.
//
// Processes started by the systemd user manager are not labelled, so the
// fix is to run outside the terminal's process tree: either as a systemd
// user service (contrib/), or by re-executing ourselves via `systemd-run --user`.

const reexecEnv = "GIT_MIRROR_MOUNT_REEXEC"

// apparmorLabel returns this process's AppArmor label ("unconfined",
// "snap.alacritty.alacritty (complain)", ...), or "" if AppArmor is not active.
var apparmorLabel = func() string { // var so tests can fake it
	for _, p := range []string{"/proc/self/attr/apparmor/current", "/proc/self/attr/current"} {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimRight(string(b), "\x00\n")
		}
	}
	return ""
}

// labelName strips the mode suffix: "snap.x.y (complain)" -> "snap.x.y".
func labelName(label string) string {
	name, _, _ := strings.Cut(label, " (")
	return name
}

func isSnapLabel(label string) bool { return strings.HasPrefix(labelName(label), "snap.") }

func isConfined(label string) bool {
	n := labelName(label)
	return n != "" && n != "unconfined"
}

// reexecArgs builds the systemd-run command line that restarts this
// program in the user's systemd session, attached to the current terminal.
func reexecArgs(self string, args []string, interactive bool, env []string) []string {
	cmd := []string{"systemd-run", "--user", "--quiet", "--collect", "--same-dir"}
	if interactive {
		cmd = append(cmd, "--pty") // Ctrl-C reaches the process via the pty
	} else {
		cmd = append(cmd, "--pipe", "--wait")
	}
	cmd = append(cmd, "--setenv="+reexecEnv+"=1")
	// The user manager has its own environment; pass on what matters.
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "TMPDIR" || k == "HOME" || strings.HasPrefix(k, "GIT_") {
			cmd = append(cmd, "--setenv="+kv)
		}
	}
	cmd = append(cmd, "--", self)
	return append(cmd, args...)
}

// maybeReexec restarts the program outside snap confinement when needed.
// It only returns if no restart happened.
func maybeReexec(allow bool) {
	label := apparmorLabel()
	if !isSnapLabel(label) || os.Getenv(reexecEnv) != "" {
		return
	}
	if !allow {
		fmt.Fprintf(os.Stderr, "warning: running under AppArmor label %q; fusermount3 will probably refuse to mount.\n%s\n",
			labelName(label), confinementHint())
		return
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: running under AppArmor label %q and systemd-run is not available.\n%s\n",
			labelName(label), confinementHint())
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	st, _ := os.Stdin.Stat()
	interactive := st != nil && st.Mode()&os.ModeCharDevice != 0
	args := reexecArgs(self, os.Args[1:], interactive, os.Environ())
	fmt.Fprintf(os.Stderr, "note: started under snap label %q; restarting via systemd-run --user so fusermount3 can mount (disable with -no-reexec)\n",
		labelName(label))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "systemd-run: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func confinementHint() string {
	return `fusermount3 is confined by AppArmor and refuses file descriptors inherited
from confined (e.g. snap) processes. Start git-mirror-mount outside that context:
  systemd-run --user --pty git-mirror-mount ...      (one-off)
  systemctl --user start git-mirror-mount@<name>     (see contrib/ in the source)
Details: journalctl -k | grep 'apparmor.*fusermount3'`
}

// mountErrorHint explains a failed mount when confinement is a likely cause.
func mountErrorHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "fusermount") {
		return ""
	}
	label := apparmorLabel()
	if isConfined(label) {
		return fmt.Sprintf("\nThis process runs under AppArmor label %q.\n%s", labelName(label), confinementHint())
	}
	return "\nIf fusermount3 printed \"file descriptor 3 is not a socket\", AppArmor is blocking it:\n" + confinementHint()
}
