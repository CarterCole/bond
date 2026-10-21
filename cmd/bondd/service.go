package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/node"
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

func validRole(r string) bool { return r == "server" || r == "hub" || r == "relay" }

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
		return errors.New("usage: bondd service install|uninstall|restart|status <server|hub|relay> [role flags]")
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
		if role != "server" {
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
		var u unit
		if runtime.GOOS == "linux" {
			u = systemdUnit(exe, role, rest)
		} else {
			u = launchdPlist(exe, role, rest)
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
		if runtime.GOOS == "linux" {
			sys("systemctl", "disable", "--now", systemdName+role)
			os.Remove("/etc/systemd/system/" + systemdName + role + ".service")
			return sys("systemctl", "daemon-reload")
		}
		exec.Command("launchctl", "bootout", "system/"+launchdLabel+role).Run()
		return os.Remove("/Library/LaunchDaemons/" + launchdLabel + role + ".plist")

	case "restart":
		// After an upgrade the unit already points at the new binary (see
		// stableExe) but the old one is still running.
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
