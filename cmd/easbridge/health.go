package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/engine"
	"easbridge/internal/version"
)

// healthHandler serves /healthz: engine state and listener liveness, no mailbox
// content (DESIGN section 6).
func healthHandler(mgr *engine.Manager, imapUp, smtpUp *atomic.Bool, contacts bool, carddavUp *atomic.Bool, tlsi *tlsInfo) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Status()
		body := map[string]any{
			"version": version.Version, "imap_listening": imapUp.Load(), "smtp_listening": smtpUp.Load(),
			"wiped": st.Wiped, "accounts": st.Accounts,
		}
		if tlsi != nil {
			body["tls_mode"], body["tls_server_expires_in_days"] = tlsi.mode, tlsi.expiresInDays()
		}
		if contacts {
			body["carddav_listening"] = carddavUp.Load()
		}
		code := http.StatusOK
		switch {
		case !imapUp.Load() || !smtpUp.Load() || (contacts && !carddavUp.Load()):
			code, body["status"] = http.StatusServiceUnavailable, "listener down"
		case st.Wiped:
			code, body["status"] = http.StatusServiceUnavailable, "wiped: remove the WIPED marker to re-enable logins"
		default:
			body["status"] = "ok"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
	})
	return mux
}

// runHealthcheck asks the local health endpoint; used as the Docker
// HEALTHCHECK because the distroless image has no shell or curl. With -v it
// also prints the /healthz JSON (counts and states, no mail content), which is
// how to read it from outside the container: docker exec easbridge /easbridge healthcheck -v
func runHealthcheck(args []string) error { return runHealthcheckTo(args, os.Stdout) }

func runHealthcheckTo(args []string, out io.Writer) error {
	verbose := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && (args[0] == "-v" || args[0] == "--verbose"):
		verbose = true
	default:
		return fmt.Errorf("healthcheck takes no arguments except -v (print the health report)")
	}
	cfg := config.Defaults()
	if v := strings.TrimSpace(os.Getenv(config.EnvHealthAddr)); v != "" {
		cfg.HealthAddr = v
	}
	host, port, err := net.SplitHostPort(cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("%s: %w", config.EnvHealthAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	defer resp.Body.Close()
	if verbose {
		var buf bytes.Buffer
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Indent(&buf, body, "", "  ") != nil {
			buf.Reset()
			buf.Write(body)
		}
		buf.WriteByte('\n')
		out.Write(buf.Bytes())
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}
