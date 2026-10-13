package easupstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/hstern/go-activesync/eas"

	"easbridge/internal/logx"
	"easbridge/internal/upstream"
)

// Mailbox is one authenticated EAS session.
type Mailbox struct {
	cfg  Config
	hc   *http.Client
	log  *slog.Logger
	user string
	pass string
	dev  string
	st   *stagedState

	cl      eas.Client // set by Open
	version string

	fetchSem chan struct{}
}

var _ upstream.Mailbox = (*Mailbox)(nil)

func newMailbox(cfg Config, hc *http.Client, c upstream.Config) (*Mailbox, error) {
	if len(c.DeviceID) != 32 {
		return nil, errors.New("easupstream: DeviceId must be 32 hex characters")
	}
	// Register the Basic blob etc. in case a library or transport error echoes it.
	logx.RegisterCredential(c.User, c.Password)
	log := logx.EAS(cfg.Logger.With("component", "eas"))
	// Each mailbox has its own client value: its own cookies, in memory only, as
	// every EAS client does (a gateway in front of Exchange, an access-policy gateway for
	// example, hands out a session cookie; a client that drops it makes the
	// gateway open a new session for every request and can exhaust its per-user
	// session limit), and the redirect policy. The transport, and with it the
	// connection pool, stays shared.
	own := *hc
	if own.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("easupstream: cookie jar: %w", err)
		}
		own.Jar = jar
	}
	if own.CheckRedirect == nil {
		own.CheckRedirect = redirectPolicy(log)
	}
	return &Mailbox{
		cfg: cfg, hc: &own, user: c.User, pass: c.Password, dev: c.DeviceID,
		log:      log,
		st:       newStagedState(c.PolicyKey),
		fetchSem: make(chan struct{}, cfg.FetchConcurrency),
	}, nil
}

// deviceInformation is what Exchange is told about this device: the bridge, as
// what it is. Nothing a phone would have (no IMEI, phone number or mobile
// operator), and no other product's name.
func (m *Mailbox) deviceInformation() eas.DeviceInformation {
	return eas.DeviceInformation{
		Model:        m.cfg.DeviceModel,
		FriendlyName: m.cfg.DeviceFriendlyName,
		OS:           m.cfg.DeviceOS,
		OSLanguage:   m.cfg.DeviceOSLanguage,
		UserAgent:    m.cfg.UserAgent,
	}
}

func (m *Mailbox) newClient(version string) (eas.Client, error) {
	di := m.deviceInformation()
	return eas.NewClient(eas.Config{
		ServerURL:  m.cfg.URL,
		Username:   m.user,
		Password:   m.pass,
		DeviceID:   m.dev,
		DeviceType: m.cfg.DeviceType,
		ASVersion:  version,
		UserAgent:  m.cfg.UserAgent,
		HTTPClient: m.hc,
		Logger:     m.log,
		State:      m.st,
		// Sent inside the initial Provision request on 14.1 and later (vendored patch 10).
		DeviceInformation: &di,
	})
}

// supported reports the preference order for the protocol version.
func versionOrder(pref string) []string {
	order := []string{pref, "16.1", "14.1", "16.0", "14.0"}
	seen := map[string]bool{}
	out := order[:0]
	for _, v := range order {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Open implements upstream.Mailbox: one OPTIONS to pick the protocol version
// (before any concurrent use, because the library writes the version without a
// lock), then the Provision handshake, which also proves the credentials.
//
// That is all it sends: OPTIONS, the policy download and the acknowledgement. The
// device information travels inside the policy download on protocol 14.1 and later
// (MS-ASPROV 2.2.2.53; vendored patch 10). Before v0.1.5 a Settings command went
// first. It did not satisfy that rule, and a server that answers any command of a
// device that is not provisioned yet with HTTP 449 made the library run a Provision
// of its own for it: two more round trips, and a second Provision after the first
// had failed.
func (m *Mailbox) Open(ctx context.Context) error {
	probe, err := m.newClient("14.1") // OPTIONS does not send the version header
	if err != nil {
		return err
	}
	opts, err := probe.Options(ctx)
	if err != nil {
		return mapError("OPTIONS", err)
	}
	for _, v := range versionOrder(m.cfg.Version) {
		if opts.Supports(v) {
			m.version = v
			break
		}
	}
	if m.version == "" {
		return &upstream.StatusError{Op: "OPTIONS", Code: 0, Perm: true,
			Reason: fmt.Sprintf("the server offers none of the protocol versions easbridge speaks (offers %v)", opts.ProtocolVersions)}
	}
	cl, err := m.newClient(m.version)
	if err != nil {
		return err
	}
	m.cl = cl
	m.log.Debug("EAS protocol version chosen", "version", m.version, "preferred", m.cfg.Version)

	// An initial Provision starts from policy key 0 (MS-ASPROV 3.1.5.1.1). Without
	// this a device without a stored key sends no X-MS-PolicyKey header at all,
	// which MS-ASPROV 3.1.5.2 lists as the cause of status 141 on a strict server;
	// with a stored key it would send a key the handshake is about to replace.
	firstProvisioning := m.st.policyKey() == ""
	_ = m.st.SetPolicyKey(ctx, "0")
	if err := mapError("Provision", cl.Provision(ctx)); err != nil {
		return err
	}
	if m.st.policyKey() == "" {
		m.log.Info("Exchange defines no device policy for this device (Provision policy status 2): nothing to acknowledge, no policy key")
	}

	// Protocol 14.0 must not carry the device information in Provision, but it can
	// be sent with a Settings command (MS-ASCMD 2.2.3.167: as soon as possible after
	// the client has been provisioned). Once per device, best effort: only a
	// rejected login or a wipe order matters here.
	if firstProvisioning && !eas.DeviceInformationInProvision(m.version) {
		err := mapError("Settings", cl.SettingsDeviceInformation(ctx, m.deviceInformation()))
		if err != nil && (errors.Is(err, upstream.ErrAuthFailed) || errors.Is(err, upstream.ErrRemoteWipe)) {
			return err
		}
		if err != nil {
			m.log.Debug("Exchange did not accept the device information (Settings)", "err", err)
		}
	}
	return nil
}

// Version returns the negotiated protocol version ("" before Open).
func (m *Mailbox) Version() string { return m.version }

// PolicyKey implements upstream.Mailbox. The "0" Open starts a handshake from is
// not a key.
func (m *Mailbox) PolicyKey() string {
	if k := m.st.policyKey(); k != "0" {
		return k
	}
	return ""
}

func (m *Mailbox) ready() error {
	if m.cl == nil {
		return errors.New("easupstream: Open has not been called")
	}
	return nil
}

// FolderSync implements upstream.Mailbox. The library recovers from status 3 by
// itself; status 9, which MS-ASCMD uses for an invalid FolderSync key, is
// handled here: reset to "0" and retry once.
func (m *Mailbox) FolderSync(ctx context.Context, key string) (*upstream.FolderDelta, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	if key == "" {
		key = "0"
	}
	full := key == "0"
	m.st.prime(eas.FolderRootID, key)
	res, err := m.cl.FolderSync(ctx)
	if err != nil && eas.IsStatusCode(err, 9) {
		m.log.Info("FolderSync key rejected (status 9); resyncing the hierarchy from scratch")
		m.st.prime(eas.FolderRootID, "0")
		full = true
		res, err = m.cl.FolderSync(ctx)
	}
	if err != nil {
		return nil, mapError("FolderSync", err)
	}
	newKey, reset := m.st.take(eas.FolderRootID)
	if reset {
		full = true // status 3: the library restarted from "0"
	}
	d := &upstream.FolderDelta{NewKey: newKey, Full: full}
	conv := func(f eas.Folder) upstream.Folder {
		return upstream.Folder{ServerID: f.ServerID, ParentID: f.ParentID, DisplayName: f.DisplayName, Type: int(f.Type)}
	}
	for _, f := range res.Added {
		d.Added = append(d.Added, conv(f))
	}
	for _, f := range res.Updated {
		d.Updated = append(d.Updated, conv(f))
	}
	d.Deleted = res.Deleted
	return d, nil
}

// Sync implements upstream.Mailbox: exactly one data round trip (NoBootstrap),
// MIME bodies inline up to MIMEInlineMax.
func (m *Mailbox) Sync(ctx context.Context, folderID, key string, o upstream.SyncOptions) (*upstream.SyncDelta, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	if key == "" {
		key = "0"
	}
	opts := eas.EmailSyncOptions{
		WindowSize:         m.cfg.WindowSize,
		NoBootstrap:        true,
		BodyType:           eas.BodyTypeMIME,
		MIMESupport:        2,
		BodyTruncationSize: m.cfg.MIMEInlineMax,
	}
	if m.cfg.Filter == 0 {
		opts.NoDateFilter = true
	} else {
		opts.DateFilter = eas.FilterType(m.cfg.Filter)
	}
	if o.HeadersOnly {
		// No header-only mode exists; a one-byte plain-text body is the closest.
		opts.BodyType, opts.MIMESupport, opts.BodyTruncationSize = eas.BodyTypePlain, 0, 1
	}
	m.st.prime(folderID, key)
	res, err := m.cl.SyncEmail(ctx, folderID, opts)
	if err != nil {
		return nil, mapError("Sync", err)
	}
	newKey, reset := m.st.take(folderID)
	return m.toDelta(newKey, res, reset, o.HeadersOnly), nil
}

func (m *Mailbox) toDelta(newKey string, res *eas.EmailSyncResult, reset, headersOnly bool) *upstream.SyncDelta {
	d := &upstream.SyncDelta{NewKey: newKey, MoreAvailable: res.MoreAvailable, Reset: reset, Deleted: res.Deleted}
	for i := range res.Added {
		d.Added = append(d.Added, m.toItem(&res.Added[i], headersOnly))
	}
	for i := range res.Changed {
		d.Changed = append(d.Changed, m.toItem(&res.Changed[i], headersOnly))
	}
	return d
}

func (m *Mailbox) toItem(e *eas.EmailItem, headersOnly bool) upstream.Item {
	it := upstream.Item{
		ServerID: e.ServerID, DateReceived: e.DateReceived, EstimatedSize: e.BodyEstimatedSize,
		Read: e.Read, ReadSet: e.ReadSet, Flagged: e.FlagStatus == 2, FlagSet: e.FlagSet,
		Subject: e.Subject, From: e.From, To: e.To, Bcc: e.Bcc,
	}
	if !headersOnly && e.BodyType == eas.BodyTypeMIME && len(e.BodyMIME) > 0 {
		it.MIME = e.BodyMIME
		// Complete unless the server says it cut the body, or the size it
		// announced is bigger than what arrived.
		it.MIMEComplete = !e.BodyTruncated && (e.BodyEstimatedSize <= 0 || e.BodyEstimatedSize <= len(e.BodyMIME))
	}
	return it
}

// FetchMIME implements upstream.Mailbox with at most FetchConcurrency calls in
// flight (DESIGN section 12: ItemOperations concurrency 2).
func (m *Mailbox) FetchMIME(ctx context.Context, folderID, serverID string) ([]byte, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	select {
	case m.fetchSem <- struct{}{}:
		defer func() { <-m.fetchSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	item, err := m.cl.FetchEmail(ctx, folderID, serverID, eas.FetchEmailOptions{BodyType: eas.BodyTypeMIME})
	if err != nil {
		return nil, mapError("Fetch", err)
	}
	if item.BodyType != eas.BodyTypeMIME || len(item.BodyMIME) == 0 {
		return nil, fmt.Errorf("Fetch: %w", upstream.ErrNoMIME)
	}
	return item.BodyMIME, nil
}

// ApplyChanges implements upstream.Mailbox using the patched
// eas.ApplyEmailChangesWith (GetChanges=0, DeletesAsMoves selectable, server
// commands in the response handed back instead of dropped).
func (m *Mailbox) ApplyChanges(ctx context.Context, folderID, key string, changes []upstream.Change, hard bool) (*upstream.ApplyResult, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	ecs := make([]eas.EmailChange, 0, len(changes))
	for _, c := range changes {
		ecs = append(ecs, eas.EmailChange{ServerID: c.ServerID, Read: c.Read, Flagged: c.Flagged, Delete: c.Delete})
	}
	m.st.prime(folderID, key)
	res, err := eas.ApplyEmailChangesWith(ctx, m.cl, folderID, ecs, eas.ApplyOptions{HardDelete: hard})
	if err != nil {
		return nil, mapError("Sync", err)
	}
	newKey, _ := m.st.take(folderID)
	out := &upstream.ApplyResult{NewKey: newKey}
	for _, r := range res.Results {
		out.Results = append(out.Results, upstream.ChangeResult{
			ServerID: r.ServerID, Status: r.Status, OK: r.Status == 1, Gone: r.Status == 8,
		})
	}
	if sc := res.ServerChanges; sc != nil {
		out.ServerChanges = m.toDelta(newKey, sc, false, false)
	}
	return out, nil
}

// MoveItems implements upstream.Mailbox. Per-item status 3 is success.
func (m *Mailbox) MoveItems(ctx context.Context, srcFolder, dstFolder string, ids []string) ([]upstream.MoveResult, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	res, err := m.cl.MoveItems(ctx, srcFolder, dstFolder, ids)
	if err != nil {
		return nil, mapError("MoveItems", err)
	}
	out := make([]upstream.MoveResult, 0, len(res))
	for _, r := range res {
		out = append(out, upstream.MoveResult{SrcServerID: r.SrcServerID, DstServerID: r.DstServerID,
			OK: r.Status == 3 && r.DstServerID != "", Status: r.Status})
	}
	return out, nil
}

// folderOp runs a hierarchy-changing call on the hierarchy key.
func (m *Mailbox) folderOp(key string, call func() (string, error)) (*upstream.FolderResult, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	m.st.prime(eas.FolderRootID, key)
	id, err := call()
	if err != nil {
		return nil, err
	}
	newKey, _ := m.st.take(eas.FolderRootID)
	return &upstream.FolderResult{ServerID: id, NewKey: newKey}, nil
}

// CreateFolder implements upstream.Mailbox.
func (m *Mailbox) CreateFolder(ctx context.Context, key, parentID, name string) (*upstream.FolderResult, error) {
	r, err := m.folderOp(key, func() (string, error) {
		res, err := m.cl.FolderCreate(ctx, parentID, name, eas.FolderTypeUserMail)
		if err != nil {
			return "", mapError("FolderCreate", err)
		}
		return res.ServerID, nil
	})
	return r, err
}

// RenameFolder implements upstream.Mailbox.
func (m *Mailbox) RenameFolder(ctx context.Context, key, serverID, newParentID, newName string) (*upstream.FolderResult, error) {
	return m.folderOp(key, func() (string, error) {
		return serverID, mapError("FolderUpdate", m.cl.FolderUpdate(ctx, serverID, newParentID, newName))
	})
}

// DeleteFolder implements upstream.Mailbox.
func (m *Mailbox) DeleteFolder(ctx context.Context, key, serverID string) (*upstream.FolderResult, error) {
	return m.folderOp(key, func() (string, error) {
		return serverID, mapError("FolderDelete", m.cl.FolderDelete(ctx, serverID))
	})
}

// statusMessagePreviouslySent is the SendMail status for a ClientId the server
// has seen in one of its last few messages (MS-ASCMD 2.2.2, status 118).
const statusMessagePreviouslySent = 118

// SendMail implements upstream.Mailbox. The ClientId is derived from the message
// (upstream.SubmissionID), so a submission the client retries is recognised by
// Exchange, which answers status 118 instead of sending it again. That answer is
// success here: the message went out with an earlier attempt.
func (m *Mailbox) SendMail(ctx context.Context, mime []byte, saveInSent bool) error {
	if err := m.ready(); err != nil {
		return err
	}
	err := m.cl.SendMail(ctx, eas.SendMailOptions{MIME: mime, SkipSaveInSent: !saveInSent, ClientID: upstream.SubmissionID(mime)})
	var se *eas.StatusError
	if errors.As(err, &se) && se.Code == statusMessagePreviouslySent {
		m.log.Warn("Exchange reports that this message was already sent (same ClientId); treating the submission as done")
		return nil
	}
	return mapError("SendMail", err)
}

// Ping implements upstream.Mailbox. go-activesync returns the result together
// with an error for statuses other than 1 and 2 (and as a plain fmt error), so
// the status is read even when err is set.
func (m *Mailbox) Ping(ctx context.Context, heartbeat time.Duration, folderIDs []string) (*upstream.PingResult, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	folders := make([]eas.PingFolder, len(folderIDs))
	for i, id := range folderIDs {
		folders[i] = eas.PingFolder{ID: id, Class: "Email"}
	}
	secs := int(heartbeat / time.Second)
	if secs < 1 {
		secs = 1
	}
	res, err := m.cl.Ping(ctx, secs, folders)
	if res != nil && res.Status != 0 {
		return &upstream.PingResult{
			Status: res.Status, Changed: res.ChangedFolders,
			Heartbeat: time.Duration(res.HeartbeatInterval) * time.Second,
		}, nil
	}
	if err != nil {
		return nil, mapError("Ping", err)
	}
	return nil, fmt.Errorf("Ping: %w", upstream.ErrUnavailable)
}

// AcknowledgeWipe implements upstream.Mailbox. A full wipe is answered with
// Provision/RemoteWipe/Status, an account-only wipe with
// Provision/AccountOnlyRemoteWipe/Status (MS-ASPROV 3.1.5.1.2.3).
func (m *Mailbox) AcknowledgeWipe(ctx context.Context, accountOnly bool) error {
	if err := m.ready(); err != nil {
		return err
	}
	if accountOnly {
		return mapError("Provision", m.cl.AcknowledgeAccountOnlyRemoteWipe(ctx, 1))
	}
	return mapError("Provision", m.cl.AcknowledgeRemoteWipe(ctx, 1))
}

// Close implements upstream.Mailbox.
func (m *Mailbox) Close() { m.hc.CloseIdleConnections() }
