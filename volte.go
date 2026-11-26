// SPDX-License-Identifier: GPL-3.0-only

package main

// VoLTE: IMS over the LTE data connection, through the modem's DATA6_CNTL QMI
// channel (rpmsg.go) and bam-dmux channel 1 (wwan1). Verified on a UZ801 with
// Orange ES (2026-11-26): registration in ~1 s, incoming call with AMR-WB.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	ims "github.com/damonto/ims-go"
	"github.com/damonto/ims-go/lte"
	"github.com/damonto/wwan-go/cdcwdm"
	"github.com/damonto/wwan-go/qcom"
	"github.com/damonto/wwan-go/qcom/qmi"
	simcard "github.com/damonto/wwan-go/sim/card"
)

const (
	volteIface    = "wwan1"
	volteAPN      = "ims"
	volteTable    = "1006" // policy routing table for sockets on wwan1
	volteNetns    = "null" // OpenStick-Builder parks wwan1-7 there (rc.local)
	volteDuplexTD = 36000  // EARFCN >= this (bands 33-48) is TDD, below FDD
)

func run(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, args[0], args[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// mmKV reads `mmcli -m any -K` into a map.
func mmKV(ctx context.Context) map[string]string {
	out, _ := run(ctx, "mmcli", "-m", "any", "-K")
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// lteReady: the modem is registered/connected on LTE (VoLTE needs it).
func lteReady(ctx context.Context) error {
	m := mmKV(ctx)
	st := m["modem.generic.state"]
	if st != "registered" && st != "connected" && st != "connecting" && st != "disconnecting" {
		return fmt.Errorf("modem is %q, not on LTE", st)
	}
	if m["modem.generic.access-technologies.value[1]"] != "lte" {
		return fmt.Errorf("modem uses %q, not LTE", m["modem.generic.access-technologies.value[1]"])
	}
	return nil
}

// qmiValue returns the value of `label: 'value'` in qmicli output.
func qmiValue(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, label+":"); ok {
			return strings.Trim(strings.TrimSpace(v), "'")
		}
	}
	return ""
}

// accessNetworkInfo builds the P-Access-Network-Info header value from the
// serving LTE cell, e.g. "3GPP-E-UTRAN-FDD;utran-cell-id-3gpp=21403" + TAC
// (4 hex) + E-UTRAN cell ID (7 hex). Format as in damonto/sigmo.
func accessNetworkInfo(ctx context.Context, qmiDev string) (string, error) {
	op := mmKV(ctx)["modem.3gpp.operator-code"]
	if len(op) < 5 || op == "--" {
		return "", errors.New("serving network code unknown")
	}
	ss, err := run(ctx, "qmicli", "-p", "-d", qmiDev, "--nas-get-serving-system")
	if err != nil {
		return "", fmt.Errorf("serving system: %w", err)
	}
	tac, err1 := strconv.ParseUint(qmiValue(ss, "LTE tracking area code"), 10, 16)
	cell, err2 := strconv.ParseUint(qmiValue(ss, "3GPP cell ID"), 10, 28)
	if err1 != nil || err2 != nil {
		return "", errors.New("serving LTE cell (TAC / cell ID) unknown")
	}
	duplex := "FDD"
	if rf, err := run(ctx, "qmicli", "-p", "-d", qmiDev, "--nas-get-rf-band-info"); err == nil {
		if ch, err := strconv.Atoi(qmiValue(rf, "Active Channel")); err == nil && ch >= volteDuplexTD && ch < 65536 {
			duplex = "TDD"
		}
	}
	return lte.NormalizeAccessNetworkInfo(fmt.Sprintf("3GPP-E-UTRAN-%s;utran-cell-id-3gpp=%s%04X%07X", duplex, op, tac, cell))
}

// ensureIface brings wwan1 into the main network namespace if the image
// parked it in the "null" one.
func ensureIface(ctx context.Context) error {
	if _, err := run(ctx, "ip", "link", "show", volteIface); err == nil {
		return nil
	}
	if out, err := run(ctx, "ip", "netns", "exec", volteNetns, "ip", "link", "set", volteIface, "netns", "1"); err != nil {
		return fmt.Errorf("%s not found (also not in netns %s): %s", volteIface, volteNetns, out)
	}
	return nil
}

// p2pNetwork puts the IMS PDN address on wwan1 as a point-to-point link to the
// WDS gateway (raw-IP: no ARP) and routes sockets bound to wwan1, or using its
// address, through their own table. The main routing table is not touched.
type p2pNetwork struct {
	st *state
}

func (n p2pNetwork) Replace(ctx context.Context, info ims.PDNNetworkInfo) (string, error) {
	n.cleanup()
	if info.LocalIPv4 == nil || info.IPv4Gateway == nil {
		return "", errors.New("IMS PDN without IPv4 address or gateway")
	}
	for _, c := range [][]string{
		{"ip", "link", "set", volteIface, "up"},
		{"ip", "addr", "add", info.LocalIPv4.String(), "peer", info.IPv4Gateway.String(), "dev", volteIface},
		{"ip", "route", "replace", "default", "via", info.IPv4Gateway.String(), "dev", volteIface, "table", volteTable},
		{"ip", "rule", "add", "oif", volteIface, "lookup", volteTable, "priority", volteTable},
		{"ip", "rule", "add", "from", info.LocalIPv4.String(), "lookup", volteTable, "priority", volteTable},
	} {
		if out, err := run(ctx, c...); err != nil {
			return "", fmt.Errorf("%s: %s", strings.Join(c, " "), out)
		}
	}
	if n.st != nil {
		var pc []string
		for _, ip := range info.PCSCFIPs {
			pc = append(pc, ip.String())
		}
		n.st.setPDN(info.LocalIPv4.String(), pc)
	}
	return volteIface, nil
}

func (p2pNetwork) cleanup() {
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := run(ctx, "ip", "rule", "del", "lookup", volteTable); err != nil {
			break
		}
	}
	_, _ = run(ctx, "ip", "route", "flush", "table", volteTable)
	_, _ = run(ctx, "ip", "addr", "flush", "dev", volteIface)
}

func (n p2pNetwork) Close() error {
	n.cleanup()
	_, _ = run(context.Background(), "ip", "link", "set", volteIface, "down")
	return nil
}

// lenientQCOMClient is lenientSIM for the VoLTE reader (which also opens the
// IMS PDN, a method of the embedded *ims.QCOMClient).
type lenientQCOMClient struct {
	*ims.QCOMClient
}

func (r lenientQCOMClient) ReadRecord(ctx context.Context, req simcard.RecordRead) ([]byte, error) {
	return lenientReadRecord(ctx, r.QCOMClient, req)
}

func (r lenientQCOMClient) ListApplications(ctx context.Context) ([]simcard.Application, error) {
	return lenientListApplications(ctx, r)
}

// openVoLTE prepares DATA6 and returns the SIM/PDN reader and the access
// config for ims-go. close releases the QMI channel (which also drops the PDN).
func openVoLTE(ctx context.Context, st *state, c sessionConfig) (simcard.Reader, ims.AccessConfig, func(), error) {
	fail := func(err error) (simcard.Reader, ims.AccessConfig, func(), error) {
		return nil, ims.AccessConfig{}, nil, err
	}
	if err := lteReady(ctx); err != nil {
		return fail(err)
	}
	pani, err := accessNetworkInfo(ctx, c.qmiDev)
	if err != nil {
		return fail(err)
	}
	if err := ensureIface(ctx); err != nil {
		return fail(err)
	}
	dev, err := data6Endpoint()
	if err != nil {
		return fail(fmt.Errorf("DATA6 QMI channel: %w", err))
	}
	conn, err := cdcwdm.Open(dev)
	if err != nil {
		return fail(fmt.Errorf("opening %s: %w", dev, err))
	}
	qc, err := qcom.NewClient(qmi.New(&rpmsgConn{Conn: conn}), qcom.WithSlot(1))
	if err != nil {
		_ = conn.Close()
		return fail(fmt.Errorf("QMI client on %s: %w", dev, err))
	}
	if err := qc.SetWDALinkLayerProtocol(ctx, qcom.WDALinkLayerRawIP); err != nil {
		_ = qc.Close()
		return fail(fmt.Errorf("raw-IP on DATA6: %w", err))
	}
	// persistent modem profile for the IMS APN; the P-CSCF comes via PCO only
	// when the profile asks for it
	idx, err := qc.WDSProfileIndex(ctx, volteAPN)
	if err != nil {
		id, cerr := qc.WDSCreateProfileWithConfig(ctx, qcom.WDSProfileConfig{Name: "ims", APN: volteAPN, PDPType: qcom.WDSPDPTypeIPv4})
		if cerr != nil {
			_ = qc.Close()
			return fail(fmt.Errorf("creating IMS profile: %w", cerr))
		}
		idx = id.Index
		c.log.Info("created modem profile for the IMS APN", "index", idx)
	}
	if err := qc.WDSModifyProfile(ctx, qcom.WDSProfileID{Type: qcom.WDSProfileType3GPP, Index: idx}, true); err != nil {
		c.log.Warn("IMS profile: P-CSCF via PCO not set", "err", err)
	}
	client, err := ims.NewQCOMClient(qc, ims.QCOMClientConfig{Network: p2pNetwork{st: st}})
	if err != nil {
		_ = qc.Close()
		return fail(fmt.Errorf("IMS QCOM client: %w", err))
	}
	reader := lenientQCOMClient{client}
	access := ims.VoLTE(lte.Config{APN: volteAPN, PDNType: "ipv4", ProfileIndex: idx, AccessNetworkInfo: pani})
	return reader, access, func() { _ = reader.Close(); p2pNetwork{}.Close() }, nil
}
