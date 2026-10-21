package portmap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const igdDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <device>
  <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <friendlyName>Gateway</friendlyName>
  <modelName>BGW320</modelName>
  <modelNumber>505</modelNumber>
  <serviceList><service>
   <serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
   <controlURL>/ctl/L3F</controlURL>
  </service></serviceList>
  <deviceList><device>
   <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
   <deviceList><device>
    <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
    <serviceList><service>
     <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
     <controlURL>/ctl/IPConn</controlURL>
    </service></serviceList>
   </device></deviceList>
  </device></deviceList>
 </device>
</root>`

type entry struct {
	client, port, lease, desc string
	byHand                    bool // made in the router's web page, not by UPnP
}

// fakeIGD is a router that speaks enough UPnP to forward ports.
type fakeIGD struct {
	t             *testing.T
	srv           *httptest.Server
	mu            sync.Mutex
	entries       map[string]entry // "UDP/7777"
	external      string
	permanentOnly bool
}

func newFakeIGD(t *testing.T) *fakeIGD {
	f := &fakeIGD{t: t, entries: map[string]entry{}, external: "203.0.113.7"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /desc.xml", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, igdDescription) })
	mux.HandleFunc("POST /ctl/IPConn", f.control)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIGD) location() string { return f.srv.URL + "/desc.xml" }

func (f *fakeIGD) entry(key string) (entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[key]
	return e, ok
}

func (f *fakeIGD) control(w http.ResponseWriter, r *http.Request) {
	_, action, _ := strings.Cut(strings.Trim(r.Header.Get("SOAPAction"), `"`), "#")
	if !strings.HasPrefix(r.Header.Get("SOAPAction"), `"urn:schemas-upnp-org:service:WANIPConnection:1#`) {
		f.t.Errorf("SOAPAction %q", r.Header.Get("SOAPAction"))
	}
	in, err := leaves(r.Body)
	if err != nil {
		f.t.Errorf("%s: unreadable request: %v", action, err)
	}
	key := in["NewProtocol"] + "/" + in["NewExternalPort"]
	f.mu.Lock()
	defer f.mu.Unlock()
	switch action {
	case "GetExternalIPAddress":
		reply(w, action, "NewExternalIPAddress", f.external)
	case "AddPortMapping":
		if f.permanentOnly && in["NewLeaseDuration"] != "0" {
			fault(w, 725, "OnlyPermanentLeasesSupported")
			return
		}
		if e, ok := f.entries[key]; ok && (e.byHand || e.client != in["NewInternalClient"]) {
			fault(w, 718, "ConflictInMappingEntry")
			return
		}
		f.entries[key] = entry{client: in["NewInternalClient"], port: in["NewInternalPort"], lease: in["NewLeaseDuration"], desc: in["NewPortMappingDescription"]}
		reply(w, action)
	case "GetSpecificPortMappingEntry":
		e, ok := f.entries[key]
		if !ok {
			fault(w, 714, "NoSuchEntryInArray")
			return
		}
		reply(w, action, "NewInternalPort", e.port, "NewInternalClient", e.client, "NewEnabled", "1")
	case "DeletePortMapping":
		if _, ok := f.entries[key]; !ok {
			fault(w, 714, "NoSuchEntryInArray")
			return
		}
		delete(f.entries, key)
		reply(w, action)
	default:
		fault(w, 401, "Invalid Action")
	}
}

func reply(w http.ResponseWriter, action string, kv ...string) {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "<%s>%s</%s>", kv[i], kv[i+1], kv[i])
	}
	fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">%s</u:%sResponse></s:Body></s:Envelope>`, action, b.String(), action)
}

func fault(w http.ResponseWriter, code int, desc string) {
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, desc)
}

func start(t *testing.T, o Options) *Mapper {
	t.Helper()
	if o.Port == 0 {
		o.Port = 7777
	}
	o.Log = quiet
	m := Start(o)
	t.Cleanup(m.Close)
	return m
}

func at(locs ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return locs, nil }
}

func waitState(t *testing.T, m *Mapper, state string) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := m.Status()
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %q, want %q: %s", st.State, state, st.Note)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardsAndRemoves(t *testing.T) {
	f := newFakeIGD(t)
	m := start(t, Options{Description: "bond aggregator", discover: at(f.location())})
	st := waitState(t, m, "forwarded")
	for _, proto := range []string{"UDP", "TCP"} {
		e, ok := f.entry(proto + "/7777")
		if !ok || e.client != "127.0.0.1" || e.port != "7777" || e.lease != "3600" || e.desc != "bond aggregator" {
			t.Fatalf("%s forward: %+v (present %v)", proto, e, ok)
		}
	}
	if st.Router != "BGW320 505 at 127.0.0.1" || st.Internet != "203.0.113.7" || st.Forward != "127.0.0.1:7777" {
		t.Fatalf("status: %+v", st)
	}
	if !strings.Contains(st.Note, "forwards port 7777 (UDP and TCP) to 127.0.0.1:7777") || strings.Contains(st.Note, "private") {
		t.Fatalf("note: %s", st.Note)
	}
	m.Close()
	if _, ok := f.entry("UDP/7777"); ok {
		t.Fatal("UDP forward left behind after Close")
	}
	if _, ok := f.entry("TCP/7777"); ok {
		t.Fatal("TCP forward left behind after Close")
	}
}

func TestExternalPortDiffers(t *testing.T) {
	f := newFakeIGD(t)
	m := start(t, Options{Port: 7777, External: 443, discover: at(f.location())})
	waitState(t, m, "forwarded")
	if e, ok := f.entry("UDP/443"); !ok || e.port != "7777" {
		t.Fatalf("forward: %+v (present %v)", e, ok)
	}
}

func TestPermanentOnlyRouter(t *testing.T) {
	f := newFakeIGD(t)
	f.permanentOnly = true
	m := start(t, Options{discover: at(f.location())})
	waitState(t, m, "forwarded")
	if e, _ := f.entry("TCP/7777"); e.lease != "0" {
		t.Fatalf("lease %q, want a permanent forward", e.lease)
	}
}

func TestForwardMadeByHandIsKept(t *testing.T) {
	f := newFakeIGD(t)
	for _, proto := range []string{"UDP", "TCP"} {
		f.entries[proto+"/7777"] = entry{client: "127.0.0.1", port: "7777", byHand: true}
	}
	m := start(t, Options{discover: at(f.location())})
	waitState(t, m, "forwarded")
	m.Close()
	if _, ok := f.entry("UDP/7777"); !ok {
		t.Fatal("removed a forward someone made by hand")
	}
}

func TestPortForwardedElsewhere(t *testing.T) {
	f := newFakeIGD(t)
	f.entries["UDP/7777"] = entry{client: "192.168.1.50", port: "7777", byHand: true}
	m := start(t, Options{discover: at(f.location())})
	st := waitState(t, m, "failed")
	if !strings.Contains(st.Note, "already forwards it to 192.168.1.50:7777") {
		t.Fatalf("note: %s", st.Note)
	}
}

func TestDoubleNAT(t *testing.T) {
	f := newFakeIGD(t)
	f.external = "192.168.0.20"
	m := start(t, Options{discover: at(f.location())})
	st := waitState(t, m, "forwarded")
	if !strings.Contains(st.Note, "192.168.0.20 is private") {
		t.Fatalf("note: %s", st.Note)
	}
}

func TestRouterFoundLater(t *testing.T) {
	f := newFakeIGD(t)
	var mu sync.Mutex
	on := false
	m := start(t, Options{retry: 10 * time.Millisecond, discover: func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !on {
			return nil, nil
		}
		return []string{f.location()}, nil
	}})
	deadline := time.Now().Add(5 * time.Second)
	for st := m.Status(); !strings.Contains(st.Note, "switch UPnP on"); st = m.Status() {
		if st.State != "searching" || time.Now().After(deadline) {
			t.Fatalf("state %q: %s", st.State, st.Note)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	on = true // UPnP switched on in the router
	mu.Unlock()
	waitState(t, m, "forwarded")
}

func TestSSDPResponse(t *testing.T) {
	resp := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n" +
		"Location: http://192.168.1.254:49152/rootDesc.xml\r\nSERVER: Linux UPnP/1.1\r\n\r\n"
	loc := ssdpLocation([]byte(resp))
	if loc != "http://192.168.1.254:49152/rootDesc.xml" {
		t.Fatalf("location %q", loc)
	}
	if !sameHost(loc, netip.MustParseAddr("192.168.1.254")) || sameHost(loc, netip.MustParseAddr("192.168.1.66")) {
		t.Fatal("sameHost")
	}
	if ssdpLocation([]byte("HTTP/1.1 200 OK\r\nST: x\r\n\r\n")) != "" {
		t.Fatal("location from a response without one")
	}
}

func TestPPPRouterWithURLBase(t *testing.T) {
	desc := `<root xmlns="urn:schemas-upnp-org:device-1-0"><URLBase>http://HOST/upnp/</URLBase><device>
		<modelName>Fritz!Box 7590</modelName>
		<deviceList><device><deviceList><device><serviceList><service>
		<serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType><controlURL>control/WANPPPConn1</controlURL>
		</service></serviceList></device></deviceList></device></deviceList></device></root>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.ReplaceAll(desc, "HOST", r.Host))
	}))
	defer srv.Close()
	gw, err := openGateway(context.Background(), srv.Client(), srv.URL+"/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if gw.control != srv.URL+"/upnp/control/WANPPPConn1" || gw.service != "urn:schemas-upnp-org:service:WANPPPConnection:1" || gw.name != "Fritz!Box 7590" {
		t.Fatalf("gateway: %+v", gw)
	}
}

func TestCloudVM(t *testing.T) {
	for _, c := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"sys_vendor": "Amazon EC2\n", "product_name": "t4g.small\n"}, "AWS"},
		{map[string]string{"sys_vendor": "Xen\n", "bios_version": "4.11.amazon\n"}, "AWS"},
		{map[string]string{"sys_vendor": "Google\n", "product_name": "Google Compute Engine\n"}, "Google Cloud"},
		{map[string]string{"sys_vendor": "Microsoft Corporation\n", "product_name": "Virtual Machine\n", "chassis_asset_tag": "7783-7084-3265-9085-8269-3286-77\n"}, "Azure"},
		// Hyper-V on a desktop at home: same vendor, no Azure tag.
		{map[string]string{"sys_vendor": "Microsoft Corporation\n", "product_name": "Virtual Machine\n", "chassis_asset_tag": "None\n"}, ""},
		{map[string]string{"sys_vendor": "ASUSTeK COMPUTER INC.\n", "product_name": "System Product Name\n"}, ""},
		{nil, ""}, // not Linux, or no DMI
	} {
		dir := t.TempDir()
		for name, content := range c.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := cloudVM(dir); got != c.want {
			t.Errorf("%v: %q, want %q", c.files, got, c.want)
		}
	}
}

// TestLiveRouter asks the real network's router what it is, read-only: it
// forwards nothing. BOND_UPNP_LIVE=1 go test -run LiveRouter -v ./core/portmap
func TestLiveRouter(t *testing.T) {
	if os.Getenv("BOND_UPNP_LIVE") == "" {
		t.Skip("set BOND_UPNP_LIVE=1 to ask this network's router")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	locs, err := discover(ctx, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) == 0 {
		t.Fatal("no UPnP router answered")
	}
	for _, loc := range locs {
		gw, err := openGateway(ctx, &http.Client{Timeout: 5 * time.Second}, loc)
		if err != nil {
			t.Logf("%s: %v", loc, err)
			continue
		}
		ext, err := gw.externalIP(ctx)
		local, _ := localAddrFor(gw.host)
		t.Logf("%s at %s: %s, Internet address %v (err %v), this machine %v", gw.name, gw.host, gw.service, ext, err, local)
	}
}

func TestControlURLOnAnotherHostRefused(t *testing.T) {
	desc := `<root><device><serviceList><service>
		<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>http://169.254.169.254/latest</controlURL>
		</service></serviceList></device></root>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, desc) }))
	defer srv.Close()
	if _, err := openGateway(context.Background(), srv.Client(), srv.URL+"/desc.xml"); err == nil {
		t.Fatal("accepted a control URL on another host")
	}
}
