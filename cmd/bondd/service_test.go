package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUnit(t *testing.T) {
	u := systemdUnit("/usr/local/bin/bondd", "hub", []string{"--name", "studio mac", "--gateway"})
	if u.path != "/etc/systemd/system/bond-hub.service" {
		t.Fatalf("path %s", u.path)
	}
	want := `ExecStart=/usr/local/bin/bondd hub --name "studio mac" --gateway`
	if !strings.Contains(u.content, want+"\n") {
		t.Fatalf("ExecStart wrong:\n%s", u.content)
	}
	for _, s := range []string{"Restart=always", "WantedBy=multi-user.target", "After=network-online.target"} {
		if !strings.Contains(u.content, s) {
			t.Errorf("missing %q", s)
		}
	}
}

func TestLaunchdPlist(t *testing.T) {
	u := launchdPlist("/usr/local/bin/bondd", "relay", []string{"--name", "A&B <mac>"})
	if u.path != "/Library/LaunchDaemons/com.cartertech.bond.relay.plist" {
		t.Fatalf("path %s", u.path)
	}
	for _, s := range []string{
		"<string>com.cartertech.bond.relay</string>",
		"<string>/usr/local/bin/bondd</string>\n\t\t<string>relay</string>\n\t\t<string>--name</string>\n\t\t<string>A&amp;B &lt;mac&gt;</string>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>",
	} {
		if !strings.Contains(u.content, s) {
			t.Errorf("missing %q in:\n%s", s, u.content)
		}
	}
}

func TestShellJoinQuoting(t *testing.T) {
	got := shellJoin([]string{"a", "b c", `d"e`, "100%", "$HOME", ""})
	want := `a "b c" "d\"e" 100%% "$$HOME" ""`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestServiceFlagHelpers(t *testing.T) {
	// The join code is consumed at install time and must not reach the unit.
	rest, code := stripFlag([]string{"--name", "n", "--join", "bond://h/T", "--gateway"}, "join")
	if code != "bond://h/T" || strings.Join(rest, " ") != "--name n --gateway" {
		t.Fatalf("stripFlag: %q %v", code, rest)
	}
	rest, code = stripFlag([]string{"-join=bond://h/T", "--joiner", "x"}, "join")
	if code != "bond://h/T" || strings.Join(rest, " ") != "--joiner x" {
		t.Fatalf("stripFlag =form: %q %v", code, rest)
	}
	if _, name := stripFlag([]string{"--gateway"}, "name"); name != "" {
		t.Fatal("found a flag that is not there")
	}
	if stateDirFrom([]string{"--state", "/tmp/x"}) != "/tmp/x" || stateDirFrom([]string{"-state=/tmp/y"}) != "/tmp/y" {
		t.Fatal("stateDirFrom")
	}
}

func TestStableExeFollowsHomebrewUpgrades(t *testing.T) {
	prefix := t.TempDir()
	keg := filepath.Join(prefix, "Cellar", "bond", "0.2.0_1")
	if err := os.MkdirAll(filepath.Join(keg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keg, "bin", "bondd"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keg, filepath.Join(prefix, "opt", "bond")); err != nil {
		t.Fatal(err)
	}
	if got, want := stableExe(filepath.Join(keg, "bin", "bondd")), filepath.Join(prefix, "opt", "bond", "bin", "bondd"); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// Anything else is left alone, including a keg without its opt link.
	for _, p := range []string{"/usr/local/bin/bondd", filepath.Join(prefix, "Cellar", "other", "1.0", "bin", "bondd")} {
		if got := stableExe(p); got != p {
			t.Errorf("stableExe(%s) = %s", p, got)
		}
	}
}

func TestRootOnly(t *testing.T) {
	// The toolchain's own files are not root's on a developer's machine; a
	// system binary is.
	mine := filepath.Join(t.TempDir(), "bondd")
	if err := os.WriteFile(mine, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && rootOnly(mine) == nil {
		t.Fatalf("%s, in a directory this user owns, passed as root's alone", mine)
	}
	if err := rootOnly("/bin/sh"); err != nil {
		t.Fatalf("/bin/sh: %v", err)
	}
	// A symlink in root's directories pointing into a user's is followed.
	if os.Geteuid() == 0 {
		link := filepath.Join("/tmp", "bond-rootonly-test")
		os.Remove(link)
		if err := os.Symlink(mine, link); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(link)
		os.Chown(mine, 501, 20)
		if rootOnly(link) == nil {
			t.Fatal("a link to a user's file passed")
		}
	}
}
