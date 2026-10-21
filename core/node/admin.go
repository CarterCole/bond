package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/web"
)

// The admin API is the local control surface: `bond status` and the
// dashboard read it, `bond path …` and friends write to it. It listens on
// loopback and is guarded against the two ways a web page in the user's
// browser could reach a loopback service: DNS rebinding (the Host header
// must be an address, not a name) and cross-site form posts (writes must be
// JSON, which a page cannot send cross-origin without a preflight we never
// answer).

func serveAdmin(addr string, routes http.Handler, log *slog.Logger) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("admin API cannot listen on %s (is another bondd running? pick another with --admin): %w", addr, err)
	}
	// Addr records where we actually ended up listening (the port may have been 0).
	srv := &http.Server{Addr: ln.Addr().String(), Handler: guard(routes), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	log.Info("dashboard and status API", "url", "http://"+srv.Addr)
	return srv, nil
}

func adminAddr(srv *http.Server) string {
	if srv == nil {
		return ""
	}
	return srv.Addr
}

// AdminAddr is where the hub's status API and dashboard are listening.
func (h *Hub) AdminAddr() string { return adminAddr(h.admin) }

// AdminAddr is where the aggregator's status API and dashboard are listening.
func (s *Server) AdminAddr() string { return adminAddr(s.admin) }

// AdminAddr is where the relay's status API is listening.
func (n *RelayNode) AdminAddr() string { return adminAddr(n.admin) }

func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err != nil && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			ct := r.Header.Get("Content-Type")
			if r.Method != http.MethodPost || !strings.HasPrefix(ct, "application/json") {
				http.Error(w, "writes must be POSTed as application/json", http.StatusUnsupportedMediaType)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errors.New("malformed JSON body")
	}
	return nil
}

// base wires up what every role serves: the dashboard and a status document.
func base(status func() any) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status()) })
	m.Handle("GET /", http.FileServerFS(web.FS))
	return m
}

func (h *Hub) adminRoutes() http.Handler {
	m := base(func() any { return h.Status() })
	m.HandleFunc("POST /api/path", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Policy string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := h.SetPolicy(req.Name, req.Policy); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, h.Status())
	})
	m.HandleFunc("POST /api/priority", func(w http.ResponseWriter, r *http.Request) {
		var rules classify.Rules
		if err := readJSON(r, &rules); err != nil {
			writeErr(w, err)
			return
		}
		if err := h.SetPriority(rules); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, h.Status())
	})
	m.HandleFunc("POST /api/relay", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Addr string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := h.AddRelay(req.Addr); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, h.Status())
	})
	return m
}

func (s *Server) adminRoutes() http.Handler {
	m := base(func() any { return s.Status() })
	// A fresh join code, optionally short-lived or single-use.
	m.HandleFunc("POST /api/code", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Note       string `json:"note"`
			TTLSeconds int    `json:"ttl_seconds"`
			MaxUses    int    `json:"max_uses"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if req.Note == "" || req.Note == "standing" {
			req.Note = "minted " + time.Now().UTC().Format(time.RFC3339)
		}
		code, err := s.reg.NewToken(req.Note, time.Duration(req.TTLSeconds)*time.Second, req.MaxUses)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"code": code.String(), "id": code.ID()})
	})
	m.HandleFunc("POST /api/kick", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID int }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := s.RemoveNode(req.ID); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, s.Status())
	})
	// The standing join code as a QR code, for phones to scan off the dashboard.
	m.HandleFunc("GET /api/qr.svg", func(w http.ResponseWriter, _ *http.Request) {
		code, err := s.reg.StandingCode()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		svg, err := QRSVG(code.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, svg)
	})
	return m
}

func (n *RelayNode) adminRoutes() http.Handler {
	return base(func() any { return n.Status() })
}

// QRSVG renders text as a QR code in SVG: black modules on white with the
// quiet zone the standard asks for, scalable to any size.
func QRSVG(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := c.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
