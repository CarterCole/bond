package portmap

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The UPnP Internet Gateway Device protocol, as much of it as forwarding a
// port takes: find the router with SSDP, read its device description for the
// WAN connection service, and call that service with SOAP.

const ssdpAddr = "239.255.255.250:1900"

var searchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

// discover multicasts an SSDP search and returns the description URLs of the
// gateways that answer within wait.
func discover(ctx context.Context, wait time.Duration) ([]string, error) {
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	for _, st := range searchTargets {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + ssdpAddr + "\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		if _, err := c.WriteToUDP([]byte(msg), dst); err != nil {
			return nil, err
		}
	}
	c.SetReadDeadline(time.Now().Add(wait))
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Now()) })
	defer stop()

	var locs []string
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			return locs, nil // the deadline: everyone who will answer has
		}
		loc := ssdpLocation(buf[:n])
		if loc == "" || seen[loc] || !sameHost(loc, from.Addr()) {
			continue
		}
		seen[loc] = true
		locs = append(locs, loc)
	}
}

// ssdpLocation is the LOCATION header of an SSDP response.
func ssdpLocation(resp []byte) string {
	for _, line := range strings.Split(string(resp), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "location") {
			if v = strings.TrimSpace(v); strings.HasPrefix(v, "http://") {
				return v
			}
		}
	}
	return ""
}

// sameHost reports whether rawURL points at addr. A router describes itself;
// anything on the LAN pointing us at some other host is ignored.
func sameHost(rawURL string, addr netip.Addr) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Unmap() == addr.Unmap()
}

// gateway is a router's WAN connection service.
type gateway struct {
	name    string // the router's model, for people
	host    string // the router's address
	control string // SOAP control URL
	service string // e.g. urn:schemas-upnp-org:service:WANIPConnection:1
	hc      *http.Client
}

type xmlDevice struct {
	FriendlyName string       `xml:"friendlyName"`
	ModelName    string       `xml:"modelName"`
	ModelNumber  string       `xml:"modelNumber"`
	Services     []xmlService `xml:"serviceList>service"`
	Devices      []xmlDevice  `xml:"deviceList>device"`
}

type xmlService struct {
	Type       string `xml:"serviceType"`
	ControlURL string `xml:"controlURL"`
}

// wanService finds the service that manages port forwards: WANIPConnection,
// or WANPPPConnection on a router that dials PPPoE itself.
func (d *xmlDevice) wanService() (xmlService, bool) {
	var ppp *xmlService
	var walk func(d *xmlDevice) *xmlService
	walk = func(d *xmlDevice) *xmlService {
		for i, s := range d.Services {
			switch {
			case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:WANIPConnection:"):
				return &d.Services[i]
			case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:WANPPPConnection:") && ppp == nil:
				ppp = &d.Services[i]
			}
		}
		for i := range d.Devices {
			if s := walk(&d.Devices[i]); s != nil {
				return s
			}
		}
		return nil
	}
	if s := walk(d); s != nil {
		return *s, true
	}
	if ppp != nil {
		return *ppp, true
	}
	return xmlService{}, false
}

func (d *xmlDevice) model() string {
	switch {
	case d.ModelName != "" && d.ModelNumber != "" && !strings.Contains(d.ModelName, d.ModelNumber):
		return d.ModelName + " " + d.ModelNumber
	case d.ModelName != "":
		return d.ModelName
	case d.FriendlyName != "":
		return d.FriendlyName
	}
	return "router"
}

// openGateway reads a router's description and finds its WAN service.
func openGateway(ctx context.Context, hc *http.Client, location string) (*gateway, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("router description %s: %s", location, resp.Status)
	}
	var root struct {
		URLBase string    `xml:"URLBase"`
		Device  xmlDevice `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&root); err != nil {
		return nil, fmt.Errorf("router description %s: %w", location, err)
	}
	svc, ok := root.Device.wanService()
	if !ok {
		return nil, fmt.Errorf("%s at %s offers no port forwarding service", root.Device.model(), location)
	}
	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if root.URLBase != "" {
		if base, err = base.Parse(root.URLBase); err != nil {
			return nil, err
		}
	}
	ctl, err := base.Parse(svc.ControlURL)
	if err != nil {
		return nil, err
	}
	host, _ := url.Parse(location)
	if ctl.Hostname() != host.Hostname() {
		return nil, fmt.Errorf("router at %s sends its control requests to another host (%s)", host.Hostname(), ctl.Host)
	}
	return &gateway{name: root.Device.model(), host: host.Hostname(), control: ctl.String(), service: svc.Type, hc: hc}, nil
}

// upnpError is a router refusing a request, with UPnP's error code.
type upnpError struct {
	Code int
	Desc string
}

func (e *upnpError) Error() string {
	if e.Desc == "" {
		return fmt.Sprintf("UPnP error %d", e.Code)
	}
	return fmt.Sprintf("%s (UPnP error %d)", e.Desc, e.Code)
}

const (
	errConflict      = 718 // ConflictInMappingEntry: forwarded to someone else
	errNoSuchEntry   = 714 // NoSuchEntryInArray
	errPermanentOnly = 725 // OnlyPermanentLeasesSupported
)

func isUPnPError(err error, code int) bool {
	var ue *upnpError
	return errors.As(err, &ue) && ue.Code == code
}

// call invokes a SOAP action. args are name, value pairs.
func (g *gateway) call(ctx context.Context, action string, args ...string) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, g.service)
	for i := 0; i+1 < len(args); i += 2 {
		b.WriteString("<" + args[i] + ">")
		xml.EscapeText(&b, []byte(args[i+1]))
		b.WriteString("</" + args[i] + ">")
	}
	fmt.Fprintf(&b, "</u:%s></s:Body></s:Envelope>", action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.control, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+g.service+"#"+action+`"`)
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	fields, perr := leaves(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		if code, err := strconv.Atoi(fields["errorCode"]); err == nil {
			return nil, &upnpError{Code: code, Desc: fields["errorDescription"]}
		}
		return nil, fmt.Errorf("%s: %s", action, resp.Status)
	}
	return fields, perr
}

// leaves collects the text of every element without children, by local name:
// all that a SOAP response or fault carries that we read.
func leaves(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	d := xml.NewDecoder(r)
	var text strings.Builder
	leaf := false
	for {
		t, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		switch t := t.(type) {
		case xml.StartElement:
			text.Reset()
			leaf = true
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if leaf {
				out[t.Name.Local] = strings.TrimSpace(text.String())
			}
			leaf = false
		}
	}
}

func (g *gateway) externalIP(ctx context.Context) (netip.Addr, error) {
	f, err := g.call(ctx, "GetExternalIPAddress")
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(f["NewExternalIPAddress"])
}

// addMapping forwards ext on the router to client:port. A router that only
// takes permanent forwards gets one; the lease actually granted is returned.
func (g *gateway) addMapping(ctx context.Context, proto string, ext int, client netip.Addr, port int, desc string, lease time.Duration) (time.Duration, error) {
	add := func(lease time.Duration) error {
		_, err := g.call(ctx, "AddPortMapping",
			"NewRemoteHost", "",
			"NewExternalPort", strconv.Itoa(ext),
			"NewProtocol", proto,
			"NewInternalPort", strconv.Itoa(port),
			"NewInternalClient", client.String(),
			"NewEnabled", "1",
			"NewPortMappingDescription", desc,
			"NewLeaseDuration", strconv.Itoa(int(lease.Seconds())))
		return err
	}
	err := add(lease)
	if isUPnPError(err, errPermanentOnly) {
		lease = 0
		err = add(0)
	}
	return lease, err
}

// mappingOwner is where ext is forwarded to already, if anywhere.
func (g *gateway) mappingOwner(ctx context.Context, proto string, ext int) (netip.AddrPort, error) {
	f, err := g.call(ctx, "GetSpecificPortMappingEntry",
		"NewRemoteHost", "",
		"NewExternalPort", strconv.Itoa(ext),
		"NewProtocol", proto)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ip, err := netip.ParseAddr(f["NewInternalClient"])
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("router reports the forward goes to %q", f["NewInternalClient"])
	}
	port, _ := strconv.ParseUint(f["NewInternalPort"], 10, 16)
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
}

func (g *gateway) deleteMapping(ctx context.Context, proto string, ext int) error {
	_, err := g.call(ctx, "DeletePortMapping",
		"NewRemoteHost", "",
		"NewExternalPort", strconv.Itoa(ext),
		"NewProtocol", proto)
	return err
}

// localAddrFor is this machine's address on the way to host: the address the
// router must forward to.
func localAddrFor(host string) (netip.Addr, error) {
	c, err := net.Dial("udp", net.JoinHostPort(host, "1900"))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}
