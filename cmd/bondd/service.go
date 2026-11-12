package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/node"
	"github.com/CarterCole/bond/core/rendezvous"
)

// A service is a role kept running by the operating system's own service
// manager — systemd on Linux, launchd on macOS — starting at boot and
// restarting if it dies. Nothing here is specific to bond beyond the names.

const (
	launchdLabel = "com.cartertech.bond."
	systemdName  = "bond-"
)

type unit struct {
	path    string // where the unit file goes
	content string
}

func validRole(r string) bool {
	return r == "server" || r == "hub" || r == "relay" || r == "rendezvous"
}

// systemdUnit renders the unit for a role.
func systemdUnit(exe, role string, args []string) unit {
	// A hub that captures the machine's traffic should come up once the
	// network has, and be restarted promptly: it is the machine's Internet.
	content := fmt.Sprintf(`[Unit]
Description=Bond %s (bonded internet)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
Restart=always
RestartSec=2
# The daemon opens a TUN device and pins sockets to interfaces.
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
StateDirectory=bond
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, role, shellJoin(append([]string{exe, role}, args...)))
	return unit{"/etc/systemd/system/" + systemdName + role + ".service", content}
}

// launchdPlist renders the launch daemon for a role.
func launchdPlist(exe, role string, args []string) unit {
	var argv strings.Builder
	for _, a := range append([]string{exe, role}, args...) {
		fmt.Fprintf(&argv, "\t\t<string>%s</string>\n", xmlEscape(a))
	}
	label := launchdLabel + role
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>2</integer>
	<key>StandardOutPath</key>
	<string>/var/log/bond-%s.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/bond-%s.log</string>
</dict>
</plist>
`, label, argv.String(), role, role)
	return unit{"/Library/LaunchDaemons/" + label + ".plist", content}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// shellJoin quotes arguments for a systemd ExecStart line.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$;&|<>()*?#~`") {
			a = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`).Replace(a) + `"`
		} else {
			a = strings.ReplaceAll(a, "%", "%%")
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func runService(args []string) error {
	if len(args) < 2 || !validRole(args[1]) {
		return errors.New("usage: bondd service install|uninstall|restart|status <server|hub|relay|rendezvous> [role flags]")
	}
	action, role, rest := args[0], args[1], args[2:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("services are managed by systemd (Linux) or launchd (macOS); on %s run bondd under your own supervisor", runtime.GOOS)
	}
	if action != "status" && os.Geteuid() != 0 {
		return errors.New("installing a system service needs root: run again with sudo")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	exe = stableExe(exe)

	switch action {
	case "install":
		// A hub or relay must have joined before it can run unattended. Join
		// here and now rather than passing the code on to the service: the
		// code is a secret, and unit files and process listings are not.
		if role == "hub" || role == "relay" {
			var code string
			rest, code = stripFlag(rest, "join")
			dir := stateDirFrom(rest)
			if code != "" {
				kind := control.KindHub
				if role == "relay" {
					kind = control.KindRelay
				}
				_, name := stripFlag(rest, "name")
				if _, err := node.LoadOrJoin(context.Background(), dir, code, name, kind); err != nil {
					return err
				}
				fmt.Println("joined the mesh; identity saved in", dir)
			} else if _, err := os.Stat(filepath.Join(dir, "node.json")); err != nil {
				return errors.New("this device has not joined a mesh yet: add --join <code>")
			}
		}
		// A rendezvous's pairing code likewise: unit files are readable by
		// all, and so is every process's command line. It goes in the state
		// directory, and the service is pointed at the file.
		if role == "server" {
			var via string
			if rest, via = stripFlag(rest, "via"); via != "" {
				if _, err := rendezvous.ParseCode(via); err != nil {
					return err
				}
				dir := stateDirFrom(rest)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				file := filepath.Join(dir, "via-code")
				if err := os.WriteFile(file, []byte(via+"\n"), 0o600); err != nil {
					return err
				}
				rest = append(rest, "--via-file", file)
				fmt.Println("pairing code saved in", file)
			}
		}
		run, err := serviceExe(exe)
		if err != nil {
			return err
		}
		var u unit
		if runtime.GOOS == "linux" {
			u = systemdUnit(run, role, rest)
		} else {
			u = launchdPlist(run, role, rest)
		}
		if err := os.WriteFile(u.path, []byte(u.content), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", u.path)
		if runtime.GOOS == "linux" {
			if err := sys("systemctl", "daemon-reload"); err != nil {
				return err
			}
			if err := sys("systemctl", "enable", systemdName+role); err != nil {
				return err
			}
			// Restart rather than start: on an upgrade the old binary is still
			// running, and start would leave it so.
			if err := sys("systemctl", "restart", systemdName+role); err != nil {
				return err
			}
			fmt.Printf("started. Logs: journalctl -u %s -f\n", systemdName+role)
		} else {
			// Replace a previous copy if there is one.
			exec.Command("launchctl", "bootout", "system/"+launchdLabel+role).Run()
			if err := sys("launchctl", "bootstrap", "system", u.path); err != nil {
				return err
			}
			fmt.Printf("started. Logs: tail -f /var/log/bond-%s.log\n", role)
		}
		fmt.Println("status:  bond status")
		return nil

	case "uninstall":
		defer removeCopyIfUnused()
		if runtime.GOOS == "linux" {
			sys("systemctl", "disable", "--now", systemdName+role)
			os.Remove("/etc/systemd/system/" + systemdName + role + ".service")
			return sys("systemctl", "daemon-reload")
		}
		exec.Command("launchctl", "bootout", "system/"+launchdLabel+role).Run()
		return os.Remove("/Library/LaunchDaemons/" + launchdLabel + role + ".plist")

	case "restart":
		// After an upgrade the unit already points at the new binary (see
		// stableExe) but the old one is still running. Or it points at the
		// copy serviceExe made, which is brought up to date first.
		if err := refreshCopy(exe); err != nil {
			return err
		}
		if runtime.GOOS == "linux" {
			return sys("systemctl", "restart", systemdName+role)
		}
		return sys("launchctl", "kickstart", "-k", "system/"+launchdLabel+role)

	case "status":
		var cmd *exec.Cmd
		if runtime.GOOS == "linux" {
			cmd = exec.Command("systemctl", "status", "--no-pager", systemdName+role)
		} else {
			cmd = exec.Command("launchctl", "print", "system/"+launchdLabel+role)
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("the %s service is not installed or not running", role)
		}
		return nil
	}
	return fmt.Errorf("unknown service action %q (want install, uninstall, restart or status)", action)
}

// stableExe maps a binary inside a Homebrew keg (…/Cellar/bond/1.2.3/bin/bondd)
// to the formula's opt link (…/opt/bond/bin/bondd), which follows upgrades.
// A unit pointing into the keg would break as soon as brew cleans up the old
// version.
func stableExe(exe string) string {
	prefix, keg, ok := strings.Cut(exe, "/Cellar/")
	if !ok {
		return exe
	}
	parts := strings.SplitN(keg, "/", 3) // formula, version, path inside the keg
	if len(parts) < 3 {
		return exe
	}
	opt := filepath.Join(prefix, "opt", parts[0], parts[2])
	if _, err := os.Stat(opt); err != nil {
		return exe
	}
	return opt
}

// A service runs as root. If its binary could be replaced by anyone else —
// a Homebrew install belongs to the user who ran brew — then anything running
// as that user could become root at the next restart. So a service only ever
// runs a binary that root alone can change: the one installing it, if it is
// that, or a copy kept where only root can write.
func copyDir() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/Bond"
	}
	return "/usr/local/libexec/bond"
}

func copyPath() string { return filepath.Join(copyDir(), "bondd") }

// serviceExe is the binary a service should run, given the one installing it.
func serviceExe(exe string) (string, error) {
	why := rootOnly(exe)
	if why == nil {
		return exe, nil
	}
	if err := copyExe(exe); err != nil {
		return "", err
	}
	fmt.Printf("%s can be changed without root (%v), so the service runs a copy: %s.\n", exe, why, copyPath())
	fmt.Println("After upgrading, `sudo bondd service restart <role>` brings the copy up to date.")
	return copyPath(), nil
}

// refreshCopy brings the copy up to date from exe, if there is a copy and
// exe is not it.
func refreshCopy(exe string) error {
	if _, err := os.Stat(copyPath()); err != nil || exe == copyPath() {
		return nil
	}
	return copyExe(exe)
}

func copyExe(exe string) error {
	dir := copyDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := rootOnly(dir); err != nil {
		return fmt.Errorf("cannot keep the service's binary where only root can change it: %w", err)
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".bondd-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), copyPath())
}

// removeCopyIfUnused removes the copy once no service is left to run it.
func removeCopyIfUnused() {
	pattern := "/etc/systemd/system/" + systemdName + "*.service"
	if runtime.GOOS == "darwin" {
		pattern = "/Library/LaunchDaemons/" + launchdLabel + "*.plist"
	}
	if left, _ := filepath.Glob(pattern); len(left) == 0 {
		os.Remove(copyPath())
		os.Remove(copyDir())
	}
}

// rootOnly reports why p could be changed by someone other than root, or
// nil if it could not: p and every directory above it, along the path as
// given and the path it resolves to, must belong to root and be writable by
// nobody else (a sticky directory, such as /tmp, aside).
func rootOnly(p string) error {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	for _, start := range []string{p, resolved} {
		for q := start; ; q = filepath.Dir(q) {
			st, err := os.Lstat(q)
			if err != nil {
				return err
			}
			// A symlink cannot be changed in place, only replaced through
			// its directory, which is checked too; where it points is
			// checked along the resolved path.
			if st.Mode()&os.ModeSymlink == 0 {
				if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
					return fmt.Errorf("%s belongs to uid %d", q, sys.Uid)
				}
				if st.Mode().Perm()&0o022 != 0 && st.Mode()&os.ModeSticky == 0 {
					return fmt.Errorf("%s can be written by others", q)
				}
			}
			if q == filepath.Dir(q) {
				break
			}
		}
	}
	return nil
}

func sys(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// stripFlag removes "--name value" or "--name=value" from args and returns
// what is left along with the value.
func stripFlag(args []string, name string) (rest []string, value string) {
	for i := 0; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		switch {
		case strings.HasPrefix(args[i], "-") && a == name && i+1 < len(args):
			value = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-") && strings.HasPrefix(a, name+"="):
			value = strings.TrimPrefix(a, name+"=")
		default:
			rest = append(rest, args[i])
		}
	}
	return rest, value
}

// stateDirFrom finds the state directory a role would use given its flags.
func stateDirFrom(args []string) string {
	for i, a := range args {
		a = strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(a, "state="); ok {
			return v
		}
		if a == "state" && i+1 < len(args) {
			return args[i+1]
		}
	}
	// A service runs as root, whoever is installing it.
	if runtime.GOOS == "darwin" {
		return "/usr/local/var/bond"
	}
	return "/var/lib/bond"
}
