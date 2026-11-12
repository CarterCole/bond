// bond is the command-line client for a running bondd: it shows what the
// bond is doing and changes how it behaves.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/CarterCole/bond/core/bond"
	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/node"
	"github.com/CarterCole/bond/core/portmap"
	"github.com/CarterCole/bond/core/rendezvous"
)

const usage = `bond — control a running bondd

Usage:
  bond status [-w] [--json]             what the bond is doing right now
  bond path <name> <policy>             primary | bonded | backup | off
  bond priority                         show what counts as priority traffic
  bond priority all | none              everything this device sends / nothing
  bond priority add <device|port>...    192.168.8.50   aa:bb:cc:dd:ee:ff   1935   udp/9000-9010
  bond priority remove <device|port>...
  bond relay add <ip:port>              use a relay that discovery did not find
  bond join <code> [--name N]           join a mesh without starting the daemon
  bond dashboard                        print the dashboard's address, with the right to make changes

On the aggregator:
  bond code [--once] [--ttl 10m]        show the join code, or mint a limited one
  bond code --rotate                    replace the join code: the old one stops working
  bond codes                            the join codes that work, by id
  bond code revoke <id>                 stop one of them working
  bond devices                          who is in the mesh
  bond kick <id>                        remove a device from the mesh

On a rendezvous, bond code shows the pairing code.

Global flags:
  --admin <addr>    the daemon's status API (default 127.0.0.1:7780, or $BOND_ADMIN)
  --state <dir>     the daemon's state directory, where its admin token is
                    (default: where bondd keeps it, or $BOND_STATE)

Showing a code and making changes need the daemon's admin token, which only
the user bondd runs as can read: use sudo when bondd runs as a service.

Path policies:
  primary   preferred while healthy
  bonded    used alongside whenever that gets traffic there sooner
  backup    idle until nothing else is healthy (saves metered data)
  off       never used
`

var (
	admin    = "127.0.0.1:7780"
	stateDir string // --state: where the daemon's admin token is
)

// globalFlag removes "--name value" or "--name=value" from anywhere in args.
func globalFlag(args []string, name string, into *string) []string {
	for i := 0; i < len(args); i++ {
		if v, ok := strings.CutPrefix(args[i], "--"+name+"="); ok {
			*into, args = v, append(args[:i:i], args[i+1:]...)
			i--
		} else if args[i] == "--"+name && i+1 < len(args) {
			*into, args = args[i+1], append(args[:i:i], args[i+2:]...)
			i--
		}
	}
	return args
}

func main() {
	args := os.Args[1:]
	if v := os.Getenv("BOND_ADMIN"); v != "" {
		admin = v
	}
	args = globalFlag(args, "admin", &admin)
	args = globalFlag(args, "state", &stateDir)
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "status":
		err = cmdStatus(rest)
	case "path":
		err = cmdPath(rest)
	case "priority":
		err = cmdPriority(rest)
	case "relay":
		err = cmdRelay(rest)
	case "join":
		err = cmdJoin(rest)
	case "code":
		err = cmdCode(rest)
	case "codes":
		err = cmdCodes()
	case "dashboard":
		err = cmdDashboard()
	case "devices":
		err = cmdDevices()
	case "kick":
		err = cmdKick(rest)
	case "version", "--version":
		fmt.Println("bond", node.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "bond: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bond:", err)
		os.Exit(1)
	}
}

var client = &http.Client{Timeout: 5 * time.Second}

// adminTokens finds the admin tokens this user can read, and the state
// directories it found one in but could not read. Without --state that is
// both where a daemon run by this user and one run as root keep theirs.
func adminTokens() (toks, denied []string) {
	if t := os.Getenv("BOND_ADMIN_TOKEN"); t != "" {
		return []string{t}, nil
	}
	dirs := []string{node.DefaultStateDir()}
	if stateDir != "" {
		dirs = []string{stateDir}
	} else if sys := node.SystemStateDir(); sys != dirs[0] {
		dirs = append(dirs, sys)
	}
	for _, d := range dirs {
		switch t, err := node.ReadAdminToken(d); {
		case err == nil:
			toks = append(toks, t)
		case errors.Is(err, fs.ErrPermission):
			denied = append(denied, d)
		}
	}
	return toks, denied
}

func needToken(denied []string) error {
	if len(denied) > 0 {
		return fmt.Errorf("this needs the daemon's admin token, in %s, which only the user bondd runs as can read: run bond with sudo", denied[0])
	}
	return errors.New("this needs the daemon's admin token: run bond as the user bondd runs as (with sudo, for a service), or point it at the daemon's state directory with --state")
}

// send makes one request to the daemon, with an admin token the daemon
// accepts if this user has one, and returns the reply's status and body.
func send(method, path string, body []byte) (status int, data []byte, asAdmin bool, err error) {
	toks, denied := adminTokens()
	for _, tok := range append(toks, "") {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, "http://"+admin+path, rd)
		if err != nil {
			return 0, nil, false, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, false, fmt.Errorf("no bondd answering at %s — is it running? (%v)", admin, unwrap(err))
		}
		data, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			if tok != "" {
				continue // another daemon's token: try the next, then none
			}
			return resp.StatusCode, data, false, needToken(denied)
		}
		return resp.StatusCode, data, resp.Header.Get("X-Bond-Admin") == "1", nil
	}
	return 0, nil, false, needToken(denied)
}

// call talks to the daemon. out, when not nil, receives the decoded reply.
func call(method, path string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	status, data, _, err := send(method, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			return errors.New("the daemon at " + admin + " is not in a role that supports this command")
		}
		return fmt.Errorf("daemon said %d %s: %s", status, http.StatusText(status), strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func unwrap(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

// status is the union of the four roles' status documents.
type status struct {
	Role string `json:"role"`
	node.HubStatus
	Nodes    []node.NodeStatus `json:"nodes"`
	Network  string            `json:"network"`
	Public   []string          `json:"public"`
	JoinCode string            `json:"join_code"`
	PortMap  *portmap.Status   `json:"port_map"`
	Relay    *struct {
		Clients   int    `json:"clients"`
		UpBytes   uint64 `json:"up_bytes"`
		DownBytes uint64 `json:"down_bytes"`
		Refused   uint64 `json:"refused"`
		Port      int    `json:"port"`
		Upstream  string `json:"upstream"`
	} `json:"relay"`
	Uplink      string                   `json:"uplink"`
	Via         *rendezvous.ClientStatus `json:"via"`
	Rendezvous  *rendezvous.ServerStatus `json:"rendezvous"`
	PairingCode string                   `json:"pairing_code"`
}

// getStatus reads the status document: with the codes in it when this user
// holds the daemon's admin token, without them otherwise.
func getStatus() (*status, []byte, error) {
	_, raw, _, err := send(http.MethodGet, "/api/status", nil)
	if err != nil {
		return nil, nil, err
	}
	var st status
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, nil, fmt.Errorf("unintelligible status from %s: %w", admin, err)
	}
	return &st, raw, nil
}

func rate(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.0f kbit/s", bps/1e3)
	}
	return fmt.Sprintf("%.0f bit/s", bps)
}

func size(b uint64) string {
	switch {
	case b >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(b)/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(b)/1e6)
	case b >= 1e3:
		return fmt.Sprintf("%.0f kB", float64(b)/1e3)
	}
	return fmt.Sprintf("%d B", b)
}

func pathTable(w io.Writer, indent string, paths []bond.PathStatus) {
	if len(paths) == 0 {
		fmt.Fprintln(w, indent+"(no paths)")
		return
	}
	// Paths that reach this aggregator through a rendezvous say which one.
	via := false
	for _, p := range paths {
		via = via || p.Via != ""
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := indent + "PATH\tKIND\tPOLICY\tSTATE\tRTT\tLOSS\tSEND CAP\tUP\tDOWN"
	if via {
		head += "\tVIA"
	}
	fmt.Fprintln(tw, head)
	for _, p := range paths {
		state, rtt, capacity := "down", "-", "-"
		switch {
		case p.Policy == "off":
			state = "off"
		case p.Up:
			state, rtt = "up", fmt.Sprintf("%.0f ms", p.RTTMs)
		case p.Kind == "ws":
			state = "connecting"
		case p.TxErrors > 0 && p.RxBytes == 0:
			// The OS refused to send: typically an interface with no route
			// to the aggregator (no default gateway of its own).
			state = "no route"
		}
		if p.CapacityBps > 0 {
			capacity = rate(p.CapacityBps)
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t%s\t%.1f%%\t%s\t%s\t%s", indent,
			p.Name, p.Kind, p.Policy, state, rtt, p.Loss*100, capacity, rate(p.TxBps), rate(p.RxBps))
		if via {
			fmt.Fprintf(tw, "\t%s", firstNonEmpty(p.Via, "direct"))
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()
}

func render(w io.Writer, st *status) {
	switch st.Role {
	case "hub":
		conn := "NOT CONNECTED"
		if st.Connected {
			conn = "connected"
		}
		fmt.Fprintf(w, "hub %s  %s  %s to %s  (%s mode", st.Name, st.Address, conn, st.Aggregator, st.Mode)
		if st.Gateway {
			fmt.Fprint(w, ", sharing with LAN")
		}
		fmt.Fprintln(w, ")")
		var tx, rx float64
		for _, p := range st.Session.Paths {
			tx, rx = tx+p.TxBps, rx+p.RxBps
		}
		fmt.Fprintf(w, "up %s   down %s   reorder window %.0f ms\n", rate(tx), rate(rx), st.Session.HoldMs)
		if st.Socks != "" {
			fmt.Fprintf(w, "no-root mode: send traffic through the SOCKS5 proxy at %s\n", st.Socks)
		}
		fmt.Fprintln(w)
		pathTable(w, "", st.Session.Paths)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "priority:", describe(st.Priority))
	case "aggregator":
		online := 0
		for _, n := range st.Nodes {
			if n.Online {
				online++
			}
		}
		fmt.Fprintf(w, "aggregator  mesh %s  %s  (%s mode)  %d of %d devices online\n",
			st.Network, strings.Join(st.Public, ", "), st.Mode, online, len(st.Nodes))
		if st.PortMap != nil {
			fmt.Fprintln(w, "router:", st.PortMap.Note)
		}
		if v := st.Via; v != nil {
			fmt.Fprintln(w, "rendezvous:", viaState(v))
		}
		fmt.Fprintln(w)
		for _, n := range st.Nodes {
			fmt.Fprintf(w, "[%d] %s  %s  %s  %s\n", n.ID, n.Name, n.Kind, n.IP, nodeState(n))
			if n.Session != nil && len(n.Session.Paths) > 0 {
				pathTable(w, "    ", n.Session.Paths)
			}
		}
		if len(st.Nodes) == 0 {
			fmt.Fprintln(w, "nothing has joined yet: bond code")
		}
	case "relay":
		fmt.Fprintf(w, "relay %s  sharing %s  listening on udp port %d  forwarding to %s\n", st.Name,
			firstNonEmpty(st.Uplink, "the default uplink"), st.Relay.Port, st.Relay.Upstream)
		fmt.Fprintf(w, "%d hub path(s) through it   forwarded up %s   down %s\n", st.Relay.Clients, size(st.Relay.UpBytes), size(st.Relay.DownBytes))
	case "rendezvous":
		r := st.Rendezvous
		fmt.Fprintf(w, "rendezvous  %s  %d aggregator(s) registered   forwarded up %s   down %s   refused %d\n",
			r.Public, len(r.Aggregators), size(r.UpBytes), size(r.DownBytes), r.Refused)
		fmt.Fprintln(w, "pairing code:", firstNonEmpty(st.PairingCode, "(needs the admin token: sudo bond code)"))
		if len(r.Aggregators) > 0 {
			fmt.Fprintln(w)
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "MESH\tLINK FROM\tCONNECTED\tDEVICES\tSOCKETS\tUP\tDOWN")
			for _, a := range r.Aggregators {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", a.Mesh, firstNonEmpty(a.Link, "(no link yet)"),
					(time.Duration(a.Connected) * time.Second).String(), a.Sessions, a.Sources, size(a.UpBytes), size(a.DownBytes))
			}
			tw.Flush()
		}
	default:
		fmt.Fprintf(w, "unknown role %q\n", st.Role)
	}
}

// viaState words an aggregator's connection to its rendezvous.
func viaState(v *rendezvous.ClientStatus) string {
	switch {
	case v.State != "registered":
		s := v.State + " to " + v.Rendezvous
		if v.Error != "" {
			s += " (last attempt: " + v.Error + ")"
		}
		return s
	case !v.LinkUp:
		return "registered with " + v.Rendezvous + ", link not answering"
	}
	s := fmt.Sprintf("registered with %s, link up", v.Rendezvous)
	if v.Seen != "" {
		s += ", seen as " + v.Seen
	}
	if len(v.Refused) > 0 {
		s += fmt.Sprintf("; %d device(s) not routed: another aggregator holds their sessions", len(v.Refused))
	}
	return s
}

// nodeState words a device's state. A relay is only ever observed through
// the hubs that use it, so it is "in use" or "idle" rather than on or off.
func nodeState(n node.NodeStatus) string {
	switch {
	case n.Kind == "relay" && n.Online:
		return "in use"
	case n.Kind == "relay":
		return "idle"
	case n.Online:
		return "online"
	}
	return "offline"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func describe(r classify.Rules) string {
	if r.All {
		return "everything this device sends"
	}
	var parts []string
	if len(r.Devices) > 0 {
		parts = append(parts, "devices "+strings.Join(r.Devices, ", "))
	}
	if len(r.Ports) > 0 {
		parts = append(parts, "ports "+strings.Join(r.Ports, ", "))
	}
	for _, d := range r.DSCP {
		parts = append(parts, "dscp "+strconv.Itoa(int(d)))
	}
	if len(parts) == 0 {
		return "none (traffic already marked DSCP EF is always honoured)"
	}
	return strings.Join(parts, "; ")
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("bond status", flag.ExitOnError)
	watch := fs.Bool("w", false, "keep refreshing")
	asJSON := fs.Bool("json", false, "print the raw status document")
	fs.Parse(args)
	for {
		st, raw, err := getStatus()
		if err != nil {
			return err
		}
		if *asJSON {
			var pretty bytes.Buffer
			json.Indent(&pretty, raw, "", "  ")
			fmt.Println(pretty.String())
			return nil
		}
		if *watch {
			fmt.Print("\033[H\033[2J")
		}
		render(os.Stdout, st)
		if !*watch {
			return nil
		}
		time.Sleep(time.Second)
	}
}

func cmdPath(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: bond path <name> <primary|bonded|backup|off>")
	}
	if err := call(http.MethodPost, "/api/path", map[string]string{"name": args[0], "policy": args[1]}, nil); err != nil {
		return err
	}
	fmt.Printf("%s is now %s\n", args[0], args[1])
	return nil
}

// isPortSpec tells "1935" and "udp/9000-9010" from device addresses.
func isPortSpec(s string) bool {
	if _, rest, ok := strings.Cut(s, "/"); ok {
		if strings.EqualFold(s[:3], "tcp") || strings.EqualFold(s[:3], "udp") {
			s = rest
		} else {
			return false // a CIDR prefix
		}
	}
	lo, hi, _ := strings.Cut(s, "-")
	if _, err := strconv.ParseUint(lo, 10, 16); err != nil {
		return false
	}
	if hi != "" {
		if _, err := strconv.ParseUint(hi, 10, 16); err != nil {
			return false
		}
	}
	return true
}

func without(list []string, drop string) []string {
	var out []string
	for _, v := range list {
		if !strings.EqualFold(v, drop) {
			out = append(out, v)
		}
	}
	return out
}

func cmdPriority(args []string) error {
	st, _, err := getStatus()
	if err != nil {
		return err
	}
	if st.Role != "hub" {
		return errors.New("priority is set on a hub")
	}
	r := st.Priority
	if len(args) == 0 {
		fmt.Println(describe(r))
		return nil
	}
	switch verb, items := args[0], args[1:]; verb {
	case "all":
		r.All = true
	case "none", "clear":
		r = classify.Rules{}
	case "add":
		if len(items) == 0 {
			return errors.New("usage: bond priority add <device|port>...")
		}
		r.All = false
		for _, it := range items {
			if isPortSpec(it) {
				r.Ports = append(without(r.Ports, it), it)
			} else {
				r.Devices = append(without(r.Devices, it), it)
			}
		}
	case "remove", "rm":
		if len(items) == 0 {
			return errors.New("usage: bond priority remove <device|port>...")
		}
		for _, it := range items {
			r.Ports, r.Devices = without(r.Ports, it), without(r.Devices, it)
		}
	default:
		return fmt.Errorf("unknown priority command %q (want all, none, add or remove)", verb)
	}
	if err := call(http.MethodPost, "/api/priority", r, nil); err != nil {
		return err
	}
	fmt.Println("priority:", describe(r))
	return nil
}

func cmdRelay(args []string) error {
	if len(args) != 2 || args[0] != "add" {
		return errors.New("usage: bond relay add <ip:port>")
	}
	if err := call(http.MethodPost, "/api/relay", map[string]string{"addr": args[1]}, nil); err != nil {
		return err
	}
	fmt.Println("added relay", args[1])
	return nil
}

func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("bond join", flag.ExitOnError)
	name := fs.String("name", "", "this device's name in the mesh (default: hostname)")
	kind := fs.String("kind", "hub", "hub or relay")
	state := fs.String("state", "", "state directory (default "+node.DefaultStateDir()+")")
	// Accept the code before or after the flags.
	var code string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		code, args = args[0], args[1:]
	}
	fs.Parse(args)
	if code == "" {
		code = fs.Arg(0)
	}
	if code == "" {
		return errors.New("usage: bond join <code> [--name N] [--kind hub|relay]")
	}
	dir := firstNonEmpty(*state, stateDir) // --state may have been taken as the global flag
	if dir == "" {
		dir = node.DefaultStateDir()
	}
	cfg, err := node.Join(context.Background(), dir, code, *name, *kind)
	if err != nil {
		return err
	}
	fmt.Printf("joined mesh %s as %s (%s). Identity saved in %s\n", cfg.Mesh.ID, cfg.Name, cfg.Mesh.Address.Addr(), dir)
	fmt.Printf("start it with:  bondd %s --state %s\n", cfg.Kind, dir)
	return nil
}

func cmdCode(args []string) error {
	if len(args) > 0 && args[0] == "revoke" {
		if len(args) != 2 {
			return errors.New("usage: bond code revoke <id>   (ids are shown by: bond codes)")
		}
		if err := call(http.MethodPost, "/api/code/revoke", map[string]string{"id": args[1]}, nil); err != nil {
			return err
		}
		fmt.Println("revoked join code", args[1], "— devices that joined with it stay in the mesh: bond kick removes one")
		return nil
	}
	fs := flag.NewFlagSet("bond code", flag.ExitOnError)
	once := fs.Bool("once", false, "mint a code that admits a single device")
	ttl := fs.Duration("ttl", 0, "mint a code that expires after this long (e.g. 10m)")
	rotate := fs.Bool("rotate", false, "replace the standing join code; the old one stops working")
	fs.Parse(args)
	var code string
	switch {
	case *rotate:
		var out struct{ Code string }
		if err := call(http.MethodPost, "/api/code/rotate", map[string]string{}, &out); err != nil {
			return err
		}
		code = out.Code
		fmt.Fprintln(os.Stderr, "The previous join code no longer works. Devices that joined with it stay in the mesh.")
	case *once || *ttl > 0:
		req := map[string]int{"ttl_seconds": int(ttl.Seconds())}
		if *once {
			req["max_uses"] = 1
		}
		var out struct{ Code, ID string }
		if err := call(http.MethodPost, "/api/code", req, &out); err != nil {
			return err
		}
		code = out.Code
		fmt.Fprintln(os.Stderr, "id", out.ID, "(bond code revoke "+out.ID+" stops it working)")
	default:
		st, _, err := getStatus()
		if err != nil {
			return err
		}
		switch st.Role {
		case "aggregator":
			code = st.JoinCode
		case "rendezvous":
			code = st.PairingCode
		default:
			return errors.New("join codes come from the aggregator: run this on the server")
		}
		if code == "" {
			_, denied := adminTokens()
			return needToken(denied)
		}
	}
	fmt.Println(code)
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 && strings.HasPrefix(code, "bond://") {
		fmt.Println()
		qrterminal.GenerateWithConfig(code, qrterminal.Config{
			Level: qrterminal.M, Writer: os.Stdout, HalfBlocks: true, QuietZone: 2,
			BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK,
			WhiteChar: qrterminal.WHITE_WHITE, BlackWhiteChar: qrterminal.BLACK_WHITE,
		})
	}
	return nil
}

func cmdCodes() error {
	var codes []control.TokenInfo
	if err := call(http.MethodGet, "/api/codes", nil, &codes); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNOTE\tCREATED\tEXPIRES\tUSES")
	for _, c := range codes {
		expires, uses := "never", strconv.Itoa(c.Uses)
		if !c.Expires.IsZero() {
			expires = c.Expires.Local().Format("2006-01-02 15:04")
		}
		if c.MaxUses > 0 {
			uses += " of " + strconv.Itoa(c.MaxUses)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Note, c.Created.Local().Format("2006-01-02 15:04"), expires, uses)
	}
	return tw.Flush()
}

// cmdDashboard prints the dashboard's address with the admin token in it,
// for a browser on this machine. The token stays in the address's fragment,
// which a browser never sends anywhere.
func cmdDashboard() error {
	toks, denied := adminTokens()
	for _, tok := range toks {
		req, _ := http.NewRequest(http.MethodGet, "http://"+admin+"/api/status", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("no bondd answering at %s — is it running? (%v)", admin, unwrap(err))
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Println("Open this in a browser on this machine (it carries the right to make changes; keep it to yourself):")
			fmt.Println()
			fmt.Println("    http://" + admin + "/#token=" + tok)
			return nil
		}
	}
	fmt.Println("Read-only dashboard: http://" + admin)
	return needToken(denied)
}

func cmdDevices() error {
	st, _, err := getStatus()
	if err != nil {
		return err
	}
	if st.Role != "aggregator" {
		return errors.New("the device list lives on the aggregator: run this on the server")
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tKIND\tPLATFORM\tADDRESS\tSTATE\tPATHS")
	for _, n := range st.Nodes {
		state, paths := nodeState(n), 0
		if n.Session != nil {
			for _, p := range n.Session.Paths {
				if p.Up {
					paths++
				}
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\n", n.ID, n.Name, n.Kind, n.Platform, n.IP, state, paths)
	}
	return tw.Flush()
}

func cmdKick(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: bond kick <id>   (ids are shown by: bond devices)")
	}
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return errors.New("the device id is a number: see bond devices")
	}
	if err := call(http.MethodPost, "/api/kick", map[string]int{"id": id}, nil); err != nil {
		return err
	}
	fmt.Println("removed device", id)
	fmt.Println("It can join again with any join code it still holds: bond code --rotate replaces the standing one, bond codes lists the rest.")
	return nil
}
