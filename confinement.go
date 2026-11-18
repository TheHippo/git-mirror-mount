package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// unitQuote quotes s as one word of a systemd command line (for -p ExecStopPost=).
func unitQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
	return `"` + s + `"`
}

// reexecArgs builds the systemd-run command line that restarts this
// program in the user's systemd session, attached to the current terminal.
// The transient unit is called <unit>.service so the wrapper can signal it,
// and, if mountpoint is set, unmounts it lazily when the service ends, however
// it ends (the same cleanup as contrib/git-mirror-mount@.service).
func reexecArgs(self string, args []string, interactive bool, env []string, unit, mountpoint string) []string {
	cmd := []string{"systemd-run", "--user", "--quiet", "--collect", "--same-dir", "--unit=" + unit}
	if interactive {
		cmd = append(cmd, "--pty") // Ctrl-C reaches the process via the pty
	} else {
		cmd = append(cmd, "--pipe", "--wait")
	}
	if mountpoint != "" {
		fm, err := exec.LookPath("fusermount3")
		if err != nil {
			fm = "/usr/bin/fusermount3"
		}
		// Via sh, NOT exec'd and with stdio redirected: started directly (or
		// via `exec`) from the service's pty, fusermount3 runs but leaves the
		// mount behind (its AppArmor profile denies inheriting the pty and
		// logs file_inherit denials), which was verified on Ubuntu with systemd 259.
		script := `"$0" -uz "$1" </dev/null >/dev/null 2>&1`
		cmd = append(cmd, "-p", "ExecStopPost=-/bin/sh -c "+unitQuote(script)+" "+unitQuote(fm)+" "+unitQuote(mountpoint))
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

// killUnit sends sig to the main process of a transient user unit. Only the
// main process: the server's git children must not get it.
var killUnit = func(unit string, sig syscall.Signal) error {
	name := map[syscall.Signal]string{syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM"}[sig]
	out, err := exec.Command("systemctl", "--user", "kill", "--kill-whom=main", "--signal="+name, unit+".service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl kill: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// maybeReexec restarts the program outside snap confinement when needed.
// It only returns if no restart happened. mountpoint is the (possibly
// relative) mountpoint argument, used for cleanup when the service ends.
//
// While the restarted instance runs, this process stays behind as its front
// end: SIGHUP means "refresh" and is forwarded to the server (so it neither
// kills this wrapper nor the server's terminal), SIGINT/SIGTERM are forwarded
// as is, and the server's exit status becomes ours.
func maybeReexec(allow bool, mountpoint string) {
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
	if abs, err := filepath.Abs(mountpoint); err == nil {
		mountpoint = abs
	}
	st, _ := os.Stdin.Stat()
	interactive := st != nil && st.Mode()&os.ModeCharDevice != 0
	unit := "git-mirror-mount-" + strconv.Itoa(os.Getpid())
	args := reexecArgs(self, os.Args[1:], interactive, os.Environ(), unit, mountpoint)
	fmt.Fprintf(os.Stderr, "note: started under snap label %q; restarting as systemd user unit %s.service so fusermount3 can mount (disable with -no-reexec)\n"+
		"note: SIGHUP (or closing this terminal) refreshes the mount; stop it with Ctrl-C or: systemctl --user stop %s.service\n",
		labelName(label), unit, unit)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The systemd-run client inherits an ignored SIGHUP, so `pkill -HUP -f
	// git-mirror-mount` (which matches its command line too) doesn't kill it
	// and detach the pty from the server. We take SIGHUP over below.
	signal.Ignore(syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "systemd-run: %v\n", err)
		os.Exit(1)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			if err := killUnit(unit, s.(syscall.Signal)); err != nil {
				fmt.Fprintf(os.Stderr, "forwarding %v: %v\n", s, err)
			}
		case err := <-done:
			if ee, ok := err.(*exec.ExitError); ok {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					os.Exit(128 + int(ws.Signal()))
				}
				os.Exit(ee.ExitCode())
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "systemd-run: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
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
