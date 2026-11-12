package node

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
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
//
// Anyone on the machine can reach loopback, so what would be harmful in
// their hands — the join and pairing codes, and every change — also needs
// the admin token. The daemon keeps it in its state directory, readable only
// by whoever runs the daemon (root, for a service). `bond` reads it from
// there, and `bond dashboard` hands it to a browser.

// AdminTokenFile is the admin token's file in a daemon's state directory.
const AdminTokenFile = "admin.token"

// ReadAdminToken reads the admin token a daemon keeps in dir.
func ReadAdminToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, AdminTokenFile))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < 32 {
		return "", fmt.Errorf("%s is damaged", filepath.Join(dir, AdminTokenFile))
	}
	return tok, nil
}

// adminToken returns the token kept in dir, making it on first use. With no
// directory it makes one that nothing outside this process will ever know,
// which leaves the API read-only.
func adminToken(dir string) (string, error) {
	if dir != "" {
		if tok, err := ReadAdminToken(dir); err == nil {
			return tok, nil
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if dir == "" {
		return tok, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(filepath.Join(dir, AdminTokenFile), []byte(tok+"\n"), 0o600)
}

func serveAdmin(addr, stateDir string, routes http.Handler, log *slog.Logger) (*http.Server, error) {
	tok, err := adminToken(stateDir)
	if err != nil {
		return nil, fmt.Errorf("admin token: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("admin API cannot listen on %s (is another bondd running? pick another with --admin): %w", addr, err)
	}
	// Addr records where we actually ended up listening (the port may have been 0).
	srv := &http.Server{Addr: ln.Addr().String(), Handler: guard(tok, routes), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	log.Info("dashboard and status API (`bond dashboard` opens it with the right to make changes)", "url", "http://"+srv.Addr)
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

type adminKey struct{}

// isAdmin reports whether the request carried the admin token.
func isAdmin(r *http.Request) bool {
	v, _ := r.Context().Value(adminKey{}).(bool)
	return v
}

const needToken = "this needs the admin token: run bond as the user bondd runs as (with sudo, for a service), or open the dashboard with `bond dashboard`"

func refuse(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="bond"`)
	writeErrStatus(w, http.StatusUnauthorized, errors.New(msg))
}

// adminOnly serves h only to requests that carried the admin token.
func adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			refuse(w, needToken)
			return
		}
		h(w, r)
	}
}

func guard(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err != nil && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// A token that is offered must be right: that is how a client
		// holding several (one per daemon on the machine) finds this one's.
		admin := false
		if a := r.Header.Get("Authorization"); a != "" {
			got, ok := strings.CutPrefix(a, "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				refuse(w, "wrong admin token")
				return
			}
			admin = true
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
			if !admin {
				refuse(w, needToken)
				return
			}
		}
		if admin {
			w.Header().Set("X-Bond-Admin", "1")
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminKey{}, admin)))
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) { writeErrStatus(w, http.StatusBadRequest, err) }

func writeErrStatus(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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

// base wires up what every role serves: the dashboard and a status document,
// which leaves out secrets unless the request carried the admin token.
func base(status func(r *http.Request) any) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, status(r)) })
	m.Handle("GET /", http.FileServerFS(web.FS))
	return m
}

func (h *Hub) adminRoutes() http.Handler {
	m := base(func(*http.Request) any { return h.Status() })
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
	m := base(func(r *http.Request) any {
		st := s.Status()
		if !isAdmin(r) {
			st.JoinCode = ""
		} else if svg, err := QRSVG(st.JoinCode); err == nil {
			st.JoinQR = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
		}
		return st
	})
	m.HandleFunc("GET /api/codes", adminOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.reg.Tokens()) }))
	// Replace the standing code: removing a device only sticks once the
	// code it joined with has stopped working.
	m.HandleFunc("POST /api/code/rotate", func(w http.ResponseWriter, _ *http.Request) {
		code, err := s.reg.RotateStanding()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"code": code.String(), "id": code.ID()})
	})
	m.HandleFunc("POST /api/code/revoke", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := s.reg.RevokeToken(req.ID); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, s.reg.Tokens())
	})
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
	// The standing join code as a QR code, for phones to scan.
	m.HandleFunc("GET /api/qr.svg", adminOnly(func(w http.ResponseWriter, _ *http.Request) {
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
	}))
	return m
}

func (n *RelayNode) adminRoutes() http.Handler {
	return base(func(*http.Request) any { return n.Status() })
}

func (n *RendezvousNode) adminRoutes() http.Handler {
	return base(func(r *http.Request) any {
		st := n.Status()
		if !isAdmin(r) {
			st.PairingCode = ""
		}
		return st
	})
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
