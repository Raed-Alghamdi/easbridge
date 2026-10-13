package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/tlsauto"
)

// renewCheckEvery is how often the automatic server certificate is looked at while running.
const renewCheckEvery = 12 * time.Hour

// tlsInfo is what /healthz says about the certificate: the mode and the days left on the one served.
type tlsInfo struct {
	mode string
	rl   *tlsauto.Reloader
}

func (t *tlsInfo) expiresInDays() int {
	leaf := t.rl.Leaf()
	if leaf == nil {
		return 0
	}
	return int(time.Until(leaf.NotAfter).Hours() / 24)
}

// tlsMode applies the rule of tlsauto.Decide to the configuration.
func tlsMode(cfg *config.Config) (tlsauto.Mode, error) {
	return tlsauto.Decide(cfg.TLSCertSet, cfg.TLSKeySet, cfg.TLSCert, cfg.TLSKey, func(p string) bool { _, err := os.Stat(p); return err == nil })
}

// autoDir is where the automatic CA and server certificate live.
func autoDir(cfg *config.Config) string { return filepath.Join(cfg.DataDir, "tls") }

// setupTLS makes the TLS configuration of the listeners: the files that were provided, exactly as
// before (and never touched), or the automatic CA and server certificate, which it also keeps
// renewed while ctx lives and exports when asked to.
func setupTLS(ctx context.Context, cfg *config.Config, log *slog.Logger, lister config.InterfaceLister, now func() time.Time, every time.Duration) (*tls.Config, *tlsInfo, error) {
	mode, err := tlsMode(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS: %w", err)
	}
	if mode == tlsauto.Provided {
		rl, err := tlsauto.NewReloader(cfg.TLSCert, cfg.TLSKey, time.Minute)
		if err != nil {
			return nil, nil, fmt.Errorf("TLS: %w", err)
		}
		if cfg.CAExportDir != "" {
			log.Warn(config.EnvCAExportDir + " is set, but the certificate is provided: there is no CA of EASBridge's to export")
		}
		if len(cfg.TLSNames) > 0 {
			log.Warn(config.EnvTLSNames + " is set, but the certificate is provided: its names are those of your files")
		}
		log.Info("TLS: using the certificate and key you provided", "cert", cfg.TLSCert, "key", cfg.TLSKey)
		return rl.TLSConfig(), &tlsInfo{mode: string(mode), rl: rl}, nil
	}

	if lister == nil {
		lister = config.SystemInterfaces
	}
	ifaces, err := lister()
	if err != nil {
		return nil, nil, fmt.Errorf("TLS: cannot list the network interfaces to name the certificate: %w", err)
	}
	host, _ := os.Hostname()
	names := tlsauto.ResolveNames(host, cfg.TLSNames, cfg.ListenIP, ifaces)
	mgr, err := tlsauto.Open(tlsauto.Options{Dir: autoDir(cfg), Names: names, Now: now, Log: log})
	if err != nil {
		return nil, nil, fmt.Errorf("TLS: %w", err)
	}
	certFile, keyFile := mgr.ServerFiles()
	rl, err := tlsauto.NewReloader(certFile, keyFile, time.Minute)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS: %w", err)
	}
	export := func() {
		if cfg.CAExportDir == "" {
			return
		}
		changed, err := mgr.ExportCA(cfg.CAExportDir)
		switch {
		case err != nil:
			log.Error("cannot write the CA certificate to "+config.EnvCAExportDir, "dir", cfg.CAExportDir, "err", err)
		case changed:
			log.Info("CA certificate written for MailFlow's backend to trust", "dir", cfg.CAExportDir, "file", filepath.Join(cfg.CAExportDir, tlsauto.CAFile))
		}
	}
	if cfg.CAExportDir != "" {
		if _, err := mgr.ExportCA(cfg.CAExportDir); err != nil {
			return nil, nil, fmt.Errorf("TLS: %s is set but the CA certificate cannot be written there: %w", config.EnvCAExportDir, err)
		}
	}
	log.Info("TLS: automatic certificate (no certificate files were provided)",
		"ca_subject", mgr.CASubject(), "ca_sha256", mgr.CAFingerprint(), "server_expires", mgr.ServerNotAfter().Format("2006-01-02"),
		"dns_names", fmt.Sprint(names.DNS), "ip_names", len(names.IPs), "dir", autoDir(cfg), "ca_export_dir", cfg.CAExportDir)
	if every <= 0 {
		every = renewCheckEvery
	}
	go mgr.Run(ctx, every, func(renewed bool, err error) {
		if renewed {
			rl.Invalidate()
			log.Info("TLS server certificate renewed from the same CA", "expires", mgr.ServerNotAfter().Format("2006-01-02"))
		}
		if err == nil {
			export()
		}
	})
	return rl.TLSConfig(), &tlsInfo{mode: string(mode), rl: rl}, nil
}

// runCA prints the CA certificate of the automatic mode, for `docker exec easbridge /easbridge ca`.
func runCA(args []string, out io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("ca takes no arguments")
	}
	cfg := config.Defaults()
	for _, v := range []struct {
		name string
		dst  *string
		set  *bool
	}{
		{config.EnvTLSCert, &cfg.TLSCert, &cfg.TLSCertSet}, {config.EnvTLSKey, &cfg.TLSKey, &cfg.TLSKeySet}, {config.EnvDataDir, &cfg.DataDir, nil},
	} {
		if s := os.Getenv(v.name); s != "" {
			*v.dst = s
			if v.set != nil {
				*v.set = true
			}
		}
	}
	mode, err := tlsMode(cfg)
	if err != nil {
		return fmt.Errorf("TLS: %w", err)
	}
	if mode == tlsauto.Provided {
		return fmt.Errorf("the certificate is provided (%s): its CA is not managed by EASBridge, take it from whoever made your certificate", cfg.TLSCert)
	}
	pem, err := tlsauto.ReadCA(autoDir(cfg))
	if err != nil {
		return fmt.Errorf("no CA certificate yet (%v): start EASBridge once, it makes the CA at its first start", err)
	}
	_, err = out.Write(pem)
	return err
}
