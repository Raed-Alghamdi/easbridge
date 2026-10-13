package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"easbridge/internal/carddav"
	"easbridge/internal/config"
	"easbridge/internal/easupstream"
	"easbridge/internal/engine"
	"easbridge/internal/imapd"
	"easbridge/internal/logx"
	"easbridge/internal/smtpd"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
	"easbridge/internal/version"
)

func runServe(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("serve takes no arguments (configuration comes from EASBRIDGE_* variables), got %q", args)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	log := logx.New(logx.Options{Level: cfg.LogLevel, Writer: os.Stderr, JSON: cfg.LogFormat == "json"})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, log, nil)
}

// serveHooks lets tests learn the bound addresses.
type serveHooks struct {
	onListening func(imap, smtp, health string)
	// onCardDAV is called with the bound CardDAV address when the contacts feature is on.
	onCardDAV func(addr string)
	// interfaces replaces the real network interfaces for the listen checks (tests).
	interfaces config.InterfaceLister
	// tlsNow and tlsEvery replace the clock of the automatic certificate and how often it is
	// looked at (tests of the renewal).
	tlsNow   func() time.Time
	tlsEvery time.Duration
	// step is called at each step of the start with its name and, once a listener is open, its
	// address; beforeServe in the goroutine of each server just before it begins to serve;
	// afterClose once the servers have been told to close. Tests stop serve, or hold a goroutine
	// back, at an exact step with them (nothing here is used in production).
	step        func(name, addr string)
	beforeServe func(name string)
	afterClose  func()
}

func (h *serveHooks) at(name, addr string) {
	if h != nil && h.step != nil {
		h.step(name, addr)
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, hooks *serveHooks) error {
	var lister config.InterfaceLister
	if hooks != nil {
		lister = hooks.interfaces
	}
	if err := cfg.CheckListenNetwork(lister); err != nil {
		return fmt.Errorf("where to listen: %w", err)
	}
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("state database: %w", err)
	}
	if store.WipeMarkerExists(cfg.DataDir) {
		log.Error("a remote wipe happened earlier: logins are refused until the WIPED marker file in the data directory is removed")
	}

	removeStaleContactData(cfg, db, log)

	factory, closeUpstream, err := newUpstream(cfg, log)
	if err != nil {
		db.Close()
		return err
	}
	defer closeUpstream()

	ec := engineConfig(cfg)
	for _, u := range cfg.AllowedUsers {
		if strings.HasPrefix(u, "@") {
			ec.DomainEntries = true // any name of the domain passes the allow-list: see engine.Config.DomainEntries
		}
	}
	tuneEngineForTests(&ec, log)
	mgr := engine.NewManager(ec, log, db, factory, cfg.UserAllowed)
	defer func() {
		mgr.Close()
		mgr.DB().Close()
	}()

	var tlsNow func() time.Time
	var tlsEvery time.Duration
	if hooks != nil {
		tlsNow, tlsEvery = hooks.tlsNow, hooks.tlsEvery
	}
	tlsCfg, tlsInfo, err := setupTLS(ctx, cfg, log, lister, tlsNow, tlsEvery)
	if err != nil {
		return err
	}
	hooks.at("tls", "")

	imapSrv := imapd.NewServer(mgr, log)
	smtpSrv := smtpd.NewServer(mgr, log, cfg.MaxMessageBytes, "easbridge")
	var imapUp, smtpUp, carddavUp atomic.Bool

	imapLn, err := tls.Listen("tcp", cfg.IMAPAddr, tlsCfg)
	if err != nil {
		return fmt.Errorf("IMAP listener: %w", err)
	}
	hooks.at("imap listener", imapLn.Addr().String())
	smtpLn, err := tls.Listen("tcp", cfg.SMTPAddr, tlsCfg)
	if err != nil {
		imapLn.Close()
		return fmt.Errorf("SMTP listener: %w", err)
	}
	hooks.at("smtp listener", smtpLn.Addr().String())
	healthLn, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		imapLn.Close()
		smtpLn.Close()
		return fmt.Errorf("health listener: %w", err)
	}
	hooks.at("health listener", healthLn.Addr().String())

	var davLn net.Listener
	var davSrv *carddav.Server
	if cfg.ContactsEnabled() {
		// Plain TCP on the one IP, TLS added by ServeTLS with the certificate of IMAP and SMTP.
		davLn, err = net.Listen("tcp", cfg.CardDAVAddr)
		if err != nil {
			imapLn.Close()
			smtpLn.Close()
			healthLn.Close()
			return fmt.Errorf("CardDAV listener: %w", err)
		}
		hooks.at("carddav listener", davLn.Addr().String())
		davSrv = carddav.New(carddav.Options{
			Login: func(ctx context.Context, user, pass string) (string, error) {
				a, err := mgr.Login(ctx, user, pass)
				if err != nil {
					return "", err
				}
				return a.User(), nil
			},
			Ready:             func(ctx context.Context, user string) { mgr.WaitHierarchy(ctx, user, 15*time.Second) },
			DB:                mgr.DB,
			ContactsBookName:  cfg.ContactsBookName,
			DirectoryBookName: cfg.DirectoryBookName,
			DirectoryEnabled:  cfg.DirectoryEnabled(),
			Log:               log,
		})
	}

	var wg sync.WaitGroup
	errc := make(chan error, 4)
	run := func(name string, up *atomic.Bool, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if hooks != nil && hooks.beforeServe != nil {
				hooks.beforeServe(name)
			}
			if up != nil {
				up.Store(true)
				defer up.Store(false)
			}
			if err := f(); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}
	hooks.at("servers", "")
	run("IMAP", &imapUp, func() error { return imapSrv.Serve(imapLn) })
	run("SMTP", &smtpUp, func() error { return smtpSrv.Serve(smtpLn) })
	if davSrv != nil {
		run("CardDAV", &carddavUp, func() error { return davSrv.ServeTLS(davLn, tlsCfg) })
	}
	healthSrv := &http.Server{Handler: healthHandler(mgr, &imapUp, &smtpUp, davSrv != nil, &carddavUp, tlsInfo), ReadHeaderTimeout: 5 * time.Second}
	run("health", nil, func() error { return healthSrv.Serve(healthLn) })

	log.Info(cfg.ListenMode(), "imap", imapLn.Addr().String(), "smtp", smtpLn.Addr().String(), "health", healthLn.Addr().String())
	if cfg.ListensOnAnyInterface() {
		log.Warn("listening on every network interface of this host because "+config.EnvAllowAnyBind+"=1; anything that can reach the host can reach the bridge",
			"imap", cfg.IMAPAddr, "smtp", cfg.SMTPAddr, "health", cfg.HealthAddr)
	}
	if davLn != nil {
		log.Info("contacts are on: read-only CardDAV", "carddav", davLn.Addr().String(), "contacts_interval", cfg.ContactsInterval.String(),
			"directory", cfg.DirectoryEnabled(), "directory_domains", strings.Join(cfg.DirectoryDomains, ","),
			"directory_lookups_per_hour", cfg.DirectoryLookupsPerHour, "directory_refresh", cfg.DirectoryRefresh.String(),
			"directory_full_sweep", cfg.DirectoryFull, "directory_full_interval", cfg.DirectoryFullInterval.String())
		if hooks != nil && hooks.onCardDAV != nil {
			hooks.onCardDAV(davLn.Addr().String())
		}
	}
	log.Info("easbridge started", "version", version.Version, "imap", imapLn.Addr().String(), "smtp", smtpLn.Addr().String(),
		"health", healthLn.Addr().String(), "eas_version", cfg.EASVersion, "filter", cfg.Filter, "upstream", cfg.Upstream)
	if hooks != nil && hooks.onListening != nil {
		hooks.onListening(imapLn.Addr().String(), smtpLn.Addr().String(), healthLn.Addr().String())
	}

	hooks.at("started", "")
	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errc:
		log.Error("a listener failed; shutting down", "err", runErr)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	healthSrv.Shutdown(shutdown)
	imapSrv.Close()
	smtpSrv.Close()
	if davSrv != nil {
		davSrv.Close()
	}
	// Close the listeners themselves as well. A server that is told to close before its goroutine
	// has reached Serve does not always close the listener it is then given (go-smtp's Serve, after
	// its Close, registers the listener and accepts for ever), and a stop that arrives while the
	// start is still going on is exactly that order. Closing a listener twice is harmless, and an
	// Accept on a closed one returns at once, which ends every Serve.
	imapLn.Close()
	smtpLn.Close()
	healthLn.Close()
	if davLn != nil {
		davLn.Close()
	}
	if hooks != nil && hooks.afterClose != nil {
		hooks.afterClose()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
		log.Error("a listener did not stop within 10 seconds; exiting anyway")
	}
	return runErr
}

func engineConfig(c *config.Config) engine.Config {
	ec := engine.DefaultConfig()
	ec.Grace = c.EngineGrace
	ec.PingHeartbeat = c.PingHeartbeat
	ec.AuthFailCooldown = c.AuthFailCooldown
	ec.MaxMessageBytes = c.MaxMessageBytes
	if c.ContactsEnabled() {
		ec.ContactsEnabled = true
		ec.ContactsInterval = c.ContactsInterval
		if c.DirectoryEnabled() {
			ec.Directory = engine.DirectoryConfig{
				Domains: c.DirectoryDomains, Spacing: c.SearchSpacing(), DailyCap: 10 * c.DirectoryLookupsPerHour, Refresh: c.DirectoryRefresh,
				Full: c.DirectoryFull, FullInterval: c.DirectoryFullInterval,
			}
		}
	}
	return ec
}

// removeStaleContactData deletes what an earlier run stored for a feature that is off
// now (contacts without EASBRIDGE_CARDDAV_ADDR, the directory without a domain), so that
// turning a feature off also removes its data. It is a cache Exchange can rebuild.
func removeStaleContactData(cfg *config.Config, db *store.DB, log *slog.Logger) {
	switch {
	case !cfg.ContactsEnabled():
		if folders, directory, err := db.PurgeContactData(); err != nil {
			log.Error("removing the contact data of an earlier run failed", "err", err)
		} else if folders+directory > 0 {
			log.Info("contacts are off: the contact data of an earlier run was removed", "contact_folders", folders, "directory_rows", directory)
		}
	case !cfg.DirectoryEnabled():
		if n, err := db.PurgeDirectoryData(); err != nil {
			log.Error("removing the directory data of an earlier run failed", "err", err)
		} else if n > 0 {
			log.Info("the directory is off: its data from an earlier run was removed", "rows", n)
		}
	case !cfg.DirectoryFull:
		// The sweep is off: its state goes, and so do the cards that only it found.
		if n, err := db.DropSweep(); err != nil {
			log.Error("removing the sweep data of an earlier run failed", "err", err)
		} else if n > 0 {
			log.Info("the full directory sweep is off: the cards only it had found were removed", "cards", n)
		}
	}
}

// newUpstream builds the mailbox backend: the real EAS client, or (devfake
// builds only) the in-memory fake.
func newUpstream(cfg *config.Config, log *slog.Logger) (upstream.Factory, func(), error) {
	if cfg.Upstream == "fake" {
		return newFakeFactory(cfg, log)
	}
	dev := cfg.Device()
	ec := easConfig(cfg.EASURL, cfg.EASVersion, cfg.EASCAFile, dev)
	ec.Filter, ec.MIMEInlineMax, ec.WindowSize = cfg.FilterType(), cfg.MIMEInlineMax, 20
	ec.MaxHeartbeat, ec.Logger = cfg.PingHeartbeat, log
	f, err := easupstream.NewFactory(ec)
	if err != nil {
		return nil, nil, err
	}
	// What Exchange is told about this device (nothing secret; the organisation
	// approves a device and may want to see exactly this in its portal).
	log.Info("device presented to Exchange (device information goes in the initial Provision request on protocol 14.1 and later)",
		"device_type", dev.Type, "model", dev.Model, "friendly_name", dev.FriendlyName, "os", dev.OS,
		"os_language", dev.OSLanguage, "user_agent", dev.UserAgent)
	return f, func() {}, nil
}
