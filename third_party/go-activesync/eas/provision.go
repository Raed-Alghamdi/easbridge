// Copyright (C) 2026 Henry Stern
// SPDX-License-Identifier: MIT

package eas

import (
	"context"
	"errors"
	"fmt"

	"github.com/hstern/go-activesync/wbxml"
)

// policyStatusNoPolicy is Policy/Status 2: "no policy defined" (MS-ASPROV 3.1.5.2).
const policyStatusNoPolicy = 2

// errNoPolicy is the internal signal that the server defined no policy.
var errNoPolicy = errors.New("eas: no provisioning policy defined")

// Provision runs the EAS provisioning handshake and persists the resulting
// policy key in the StateStore. Most servers refuse all other commands
// until this has succeeded.
//
// The handshake is two HTTP round-trips:
//
//  1. Send Provision with PolicyKey="0"; server returns a temporary key
//     plus the policy contents (which the server expects the client to
//     evaluate but which we accept unconditionally — there is no useful
//     "decline" path for an automated client).
//  2. Send Provision again with the temporary key, acknowledging the
//     policy with Status=1; server returns the final key.
//
// The final key is then persisted and sent in the X-MS-PolicyKey header on
// every subsequent request.
//
// Provision is safe to call repeatedly; idempotent re-provisioning is the
// recommended response to a 449 status from any other command.
func (c *httpClient) Provision(ctx context.Context) error {
	// Phase 1: initial request, PolicyKey "0" means "I have no key yet".
	tempKey, policyType, err := c.provisionPhase(ctx, "0", false)
	if errors.Is(err, errNoPolicy) {
		// easbridge patch 1: Policy Status 2 means "no policy defined; stop
		// sending policy information" (MS-ASPROV 3.1.5.2). That is a
		// successful provisioning with nothing to acknowledge. Clear any
		// stale key so no X-MS-PolicyKey header is sent. Upstream issue #21.
		c.policyMu.Lock()
		c.lastPolicy = nil
		c.policyMu.Unlock()
		if serr := c.cfg.State.SetPolicyKey(ctx, ""); serr != nil {
			return fmt.Errorf("eas: Provision: clear policy key: %w", serr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("eas: Provision (phase 1): %w", err)
	}
	if tempKey == "" {
		return errors.New("eas: Provision: server returned no temporary policy key")
	}

	// Persist the temp key so the phase-2 request goes out with the right
	// X-MS-PolicyKey header (some servers check this).
	if err := c.cfg.State.SetPolicyKey(ctx, tempKey); err != nil {
		return fmt.Errorf("eas: Provision: persist temp key: %w", err)
	}

	// Phase 2: acknowledge the policy and receive the final key.
	finalKey, _, err := c.provisionPhase(ctx, tempKey, true)
	if err != nil {
		return fmt.Errorf("eas: Provision (phase 2, type=%q): %w", policyType, err)
	}
	if finalKey == "" {
		return errors.New("eas: Provision: server returned no final policy key after acknowledgement")
	}

	if err := c.cfg.State.SetPolicyKey(ctx, finalKey); err != nil {
		return fmt.Errorf("eas: Provision: persist final key: %w", err)
	}
	return nil
}

// provisionPhase issues one Provision request. ack=false sends the initial
// request; ack=true sends the acknowledgement (with Status=1).
//
// Returns the policy key and policy type from the response. The caller
// distinguishes phase 1 (key is temporary, needs ack) from phase 2 (key
// is final).
func (c *httpClient) provisionPhase(ctx context.Context, key string, ack bool) (string, string, error) {
	const policyType = "MS-EAS-Provisioning-WBXML"

	policy := wbxml.E(wbxml.PageProvision, "Policy",
		wbxml.E(wbxml.PageProvision, "PolicyType", wbxml.Text(policyType)),
	)
	if ack {
		policy.Children = append(policy.Children,
			wbxml.E(wbxml.PageProvision, "PolicyKey", wbxml.Text(key)),
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
		)
	}
	root := wbxml.E(wbxml.PageProvision, "Provision")
	// easbridge patch 10: the initial request (the policy download, not the
	// acknowledgement) of protocol 14.1 and later carries the device information
	// in front of Policies (MS-ASPROV 2.2.2.53 and 3.1.5.1.1: DeviceInformation,
	// then Policies/Policy/PolicyType). Without it Exchange answers status 165.
	if !ack {
		if di := c.initialDeviceInformation(); di != nil {
			root.Children = append(root.Children, di)
		}
	}
	root.Children = append(root.Children, wbxml.E(wbxml.PageProvision, "Policies", policy))
	doc := &wbxml.Document{Root: root}

	resp, err := c.post(ctx, "Provision", doc)
	if err != nil {
		return "", "", err
	}
	if resp == nil || resp.Root == nil {
		return "", "", errors.New("empty response")
	}
	// easbridge patch 10: the server answers the device information with a Status
	// of its own. It is cosmetic (the device list in the admin tools), so a
	// refusal is logged and does not fail the provisioning.
	if !ack {
		if di := resp.Root.Find("DeviceInformation"); di != nil {
			if st := di.Find("Status"); st != nil {
				if code := atoi(st.TextContent()); code != 0 && code != StatusOK {
					c.cfg.Logger.Warn("eas: Provision: the server did not accept the device information", "status", code)
				}
			}
		}
	}

	// easbridge patch 2: a RemoteWipe instruction can arrive with or without
	// a Policy element, so check for it before anything else. Patch 8: so can
	// the 16.1 AccountOnlyRemoteWipe (a full wipe, if both came, is the stronger).
	if RemoteWipeRequested(resp) {
		return "", "", ErrRemoteWipe
	}
	if AccountOnlyRemoteWipeRequested(resp) {
		return "", "", ErrAccountOnlyRemoteWipe
	}
	if st := topStatus(resp.Root); st != 0 && st != StatusOK {
		return "", "", &StatusError{Command: "Provision", Code: st}
	}
	respPolicy := resp.Root.Find("Policy")
	if respPolicy == nil {
		return "", "", errors.New("response missing <Policy>")
	}
	if pst := respPolicy.Find("Status"); pst != nil {
		if code := atoi(pst.TextContent()); code == policyStatusNoPolicy && !ack {
			return "", "", errNoPolicy // easbridge patch 1
		} else if code != 0 && code != StatusOK {
			return "", "", &StatusError{Command: "Provision/Policy", Code: code}
		}
	}
	keyOut := ""
	if k := respPolicy.Find("PolicyKey"); k != nil {
		keyOut = k.TextContent()
	}
	typ := policyType
	if t := respPolicy.Find("PolicyType"); t != nil {
		typ = t.TextContent()
	}
	// Capture the policy fields. Phase 1 carries the policy doc; phase 2
	// (the ack) typically does not. Either way we cache whatever's there.
	if doc := respPolicy.Find("EASProvisionDoc"); doc != nil {
		if parsed := parsePolicy(doc); parsed != nil {
			c.policyMu.Lock()
			c.lastPolicy = parsed
			c.policyMu.Unlock()
		}
	}
	return keyOut, typ, nil
}

// initialDeviceInformation returns the settings:DeviceInformation element for
// the initial Provision request, or nil when none is configured or the protocol
// version does not allow it (easbridge patch 10).
func (c *httpClient) initialDeviceInformation() *wbxml.Element {
	if c.cfg.DeviceInformation == nil || !DeviceInformationInProvision(c.cfg.ASVersion) {
		return nil
	}
	return deviceInformationElement(*c.cfg.DeviceInformation)
}

// topStatus returns the integer value of the immediate <Status> child of
// the response root, or 0 if missing.
func topStatus(root *wbxml.Element) int {
	for _, c := range root.Children {
		if e, ok := c.(*wbxml.Element); ok && e.Name == "Status" {
			return atoi(e.TextContent())
		}
	}
	return 0
}

// RemoteWipeRequested reports whether the most recent Provision response
// contained a <RemoteWipe> element instructing the client to wipe its
// data. Callers should call AcknowledgeRemoteWipe to confirm the wipe
// has been (or will be) carried out, after which the server will refuse
// further commands until a new device id provisions cleanly.
//
// Non-device callers (sync daemons, server-side agents) typically
// can't carry out a wipe in any literal sense; the conventional
// response is to delete any locally persisted state for the account
// before acknowledging.
func RemoteWipeRequested(provisionResp *wbxml.Document) bool {
	if provisionResp == nil || provisionResp.Root == nil {
		return false
	}
	return provisionResp.Root.Find("RemoteWipe") != nil
}

// AccountOnlyRemoteWipeRequested reports whether a Provision response contains
// the EAS 16.1 <AccountOnlyRemoteWipe> element (easbridge patch 8).
func AccountOnlyRemoteWipeRequested(provisionResp *wbxml.Document) bool {
	if provisionResp == nil || provisionResp.Root == nil {
		return false
	}
	return provisionResp.Root.Find("AccountOnlyRemoteWipe") != nil
}

// AcknowledgeAccountOnlyRemoteWipe confirms an account-only wipe (Status=1;
// 2 reports failure) with <Provision><AccountOnlyRemoteWipe><Status> as
// MS-ASPROV 3.1.5.1.2.3 prescribes (easbridge patch 8). The plain RemoteWipe
// acknowledgement does not answer this order.
func (c *httpClient) AcknowledgeAccountOnlyRemoteWipe(ctx context.Context, status int) error {
	if status == 0 {
		status = 1
	}
	doc := &wbxml.Document{
		Root: wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageProvision, "AccountOnlyRemoteWipe",
				wbxml.E(wbxml.PageProvision, "Status", wbxml.Text(itoa(status))),
			),
		),
	}
	resp, err := c.post(ctx, "Provision", doc)
	if err != nil {
		return err
	}
	if resp == nil || resp.Root == nil {
		return nil
	}
	if st := topStatus(resp.Root); st != 0 && st != StatusOK {
		return &StatusError{Command: "Provision/AccountOnlyRemoteWipe", Code: st}
	}
	return nil
}

// AcknowledgeRemoteWipe sends a Provision request that confirms the
// wipe was performed (Status=1). Use status=2 to report failure.
//
// This is a separate helper because the cleanup the caller wants to
// perform first is application-level: deleting per-account persistent
// state (sync keys, cached credentials, attachment caches, etc.)
// before acknowledging the wipe.
func (c *httpClient) AcknowledgeRemoteWipe(ctx context.Context, status int) error {
	if status == 0 {
		status = 1 // assume success if unspecified
	}
	doc := &wbxml.Document{
		Root: wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageProvision, "RemoteWipe",
				wbxml.E(wbxml.PageProvision, "Status", wbxml.Text(itoa(status))),
			),
		),
	}
	resp, err := c.post(ctx, "Provision", doc)
	if err != nil {
		return err
	}
	if resp == nil || resp.Root == nil {
		return nil
	}
	if st := topStatus(resp.Root); st != 0 && st != StatusOK {
		return &StatusError{Command: "Provision/RemoteWipe", Code: st}
	}
	return nil
}
