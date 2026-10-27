package main

// The Wi-Fi provider: the unit on a phone's hotspot or another access
// point, through its own radio. The provider only associates: our
// wpa_supplicant (wpa.go) joins the saved networks on the Wi-Fi interface,
// and while it is joined the uplink scan (scan.go) opens the interface like
// any other device. DHCP, routing, uid rules, firewall and the lease are the
// shared ones (session.go).
//
// On iDrive 8 the radio is the NXP 88W9098's second adapter, wlan1 (phy1):
// the first adapter's wlan0 never sends an association request. The stock
// driver (moal, host_mlme=1) reports the association as a connect event,
// which upstream wpa_supplicant ignores in SME mode; our build takes it
// (wpa/wpa-mgu-sme-connect.patch). Both radios come rfkill soft-blocked:
// the provider unblocks the interface's own phy only and blocks it again
// when Wi-Fi is switched off or the service stops. connman on the unit
// manages only the CarPlay interfaces (connmand -i wfd0,p2p-wfd0-0,
// p2p-wfd0-1), so the interface keeps its name. An iPhone's hotspot is
// seen with Maximise Compatibility on (2.4 GHz).
//
// Wi-Fi is off until switched on (wifi = on, or a wifi-enable request);
// while off, the provider leaves the radio and the interface alone. A
// wpa_supplicant of someone else's on the interface is left alone too
// (link "busy").

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	wifiIfname = "wlan1"
	kindWiFi   = "wifi"
)

// Replaced by tests.
var (
	wifiEvery     = 2 * time.Second  // the provider looks at the supplicant this often
	joinTimeout   = 40 * time.Second // a connect request that has not joined by then failed
	scanShown     = 8 * time.Second  // a scan request shows "scanning" this long
	wifiDropAfter = 6 * time.Second  // a link ends once the network is gone this long
)

type wifiNetwork struct {
	SSID    string `json:"ssid"`
	Signal  int    `json:"signal"` // dBm
	Secure  bool   `json:"secure"`
	Saved   bool   `json:"saved"`
	Current bool   `json:"current"`
}

// wifiStatus is status.json's "wifi" (devstatus.go). link: off, nohw (no
// radio), busy (another wpa_supplicant runs on it), failed, idle,
// scanning, connecting, connected (joined; the uplink uses another device
// or waits for DHCP), online (the uplink has a lease through it).
type wifiStatus struct {
	Enabled  bool          `json:"enabled"`
	Link     string        `json:"link"`
	SSID     string        `json:"ssid,omitempty"`
	IP       string        `json:"ip,omitempty"`
	Signal   *int          `json:"signal,omitempty"`
	Networks []wifiNetwork `json:"networks"`
	Saved    []string      `json:"saved"`
	Scanning bool          `json:"scanning"`
	Error    *string       `json:"error"`
}

// wifiRecord is what the provider changed, kept in the run dir so that a
// start after a killed run puts it back (restoreWiFi).
type wifiRecord struct {
	Radio []string  `json:"radio"` // rfkill soft files it unblocked
	Link  linkState `json:"link"`
}

type wifiProvider struct {
	iface  string
	sys    string // /sys/class/net
	proc   string // /proc
	record string
	wpa    *supplicant
	// linkUp turns IPv6 off on the interface and brings it up, and returns
	// how it was.
	linkUp func(iface string) (linkState, error)
	// notify, when set, is called once the provider takes or gives back the
	// interface (the service's firewall guards it meanwhile).
	notify func()

	wake  chan struct{}
	stopc chan struct{}
	done  chan struct{}
	reqMu sync.Mutex // one request or tick talks to the supplicant at a time

	mu        sync.Mutex
	enabled   bool
	owned     bool // the radio and the interface are ours
	st        wifiStatus
	joining   string
	joinNew   bool // joining a network that was not saved
	joinedAt  time.Time
	scanUntil time.Time
	err       string // a failed join, shown until the next request
	ip        string // the uplink's lease on the interface
	link      *wifiLink
}

func newWiFiProvider(c config, bin string) *wifiProvider {
	return &wifiProvider{
		iface: c.WiFiIf, sys: sysClassNet, proc: "/proc", record: c.wifiFile(), linkUp: wifiLinkUp,
		wpa:  &supplicant{bin: bin, iface: c.WiFiIf, conf: c.wpaConf(), ctrl: c.wpaCtrl(), local: c.wpaLocal()},
		wake: make(chan struct{}, 1), stopc: make(chan struct{}), done: make(chan struct{}),
		enabled: c.WiFi,
		st:      wifiStatus{Enabled: c.WiFi, Link: "off", Networks: []wifiNetwork{}, Saved: []string{}},
	}
}

func (p *wifiProvider) start() { go p.loop() }

// stop ends the provider and puts the radio and interface back.
func (p *wifiProvider) stop() {
	close(p.stopc)
	<-p.done
	p.reqMu.Lock()
	p.release()
	p.reqMu.Unlock()
}

func (p *wifiProvider) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *wifiProvider) loop() {
	defer close(p.done)
	t := time.NewTicker(wifiEvery)
	defer t.Stop()
	for {
		p.tick()
		select {
		case <-p.stopc:
			return
		case <-p.wake:
		case <-t.C:
		}
	}
}

func (p *wifiProvider) tick() {
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	// a bad reply never ends the uplink
	defer func() {
		if r := recover(); r != nil {
			logf("wifi: %v", r)
		}
	}()
	p.mu.Lock()
	st := wifiStatus{Enabled: p.enabled, Networks: []wifiNetwork{}, Saved: []string{}}
	p.mu.Unlock()
	switch {
	case !st.Enabled:
		p.release()
		st.Link = "off"
	case !exists(filepath.Join(p.sys, p.iface)):
		p.release()
		st.Link = "nohw"
	default:
		p.up(&st)
	}
	p.publish(st)
}

func (p *wifiProvider) up(st *wifiStatus) {
	fail := func(link string, err error) {
		msg := err.Error()
		st.Link, st.Error = link, &msg
	}
	if !p.wpa.isRunning() {
		for _, own := range supplicants(p.proc, p.iface, p.wpa.conf) {
			if !own {
				p.release()
				fail("busy", fmt.Errorf("another wpa_supplicant runs on %s", p.iface))
				return
			}
		}
	}
	if err := p.own(); err != nil {
		fail("failed", err)
		return
	}
	if err := p.wpa.ensure(p.proc); err != nil {
		fail("failed", err)
		return
	}
	ws := p.wpa.status()
	st.Networks, st.Saved = p.wpa.networks()
	switch ws["wpa_state"] {
	case "COMPLETED":
		st.Link = "connected"
	case "AUTHENTICATING", "ASSOCIATING", "ASSOCIATED", "4WAY_HANDSHAKE", "GROUP_HANDSHAKE":
		st.Link = "connecting"
	case "SCANNING":
		st.Link = "scanning"
	default:
		st.Link = "idle"
	}
	if st.Link == "connected" || st.Link == "connecting" {
		st.SSID = ws["ssid"]
	}
	now := time.Now()
	p.mu.Lock()
	st.Scanning = now.Before(p.scanUntil)
	joining, joinNew, joinedAt := p.joining, p.joinNew, p.joinedAt
	p.mu.Unlock()
	if joining != "" {
		switch {
		case st.Link == "connected" && st.SSID == joining:
			if err := p.wpa.joined(); err != nil {
				logf("wifi: save %q: %v", joining, err)
			}
			logf("wifi: joined %q", joining)
			joining = ""
		case now.Sub(joinedAt) > joinTimeout:
			p.wpa.reload()
			p.mu.Lock()
			p.err = fmt.Sprintf("could not join %s (wrong password, or out of range)", joining)
			p.mu.Unlock()
			logf("wifi: could not join %q", joining)
			joining = ""
		default:
			if st.Link != "connected" {
				st.Link, st.SSID = "connecting", joining
			}
			if joinNew {
				st.Saved = without(st.Saved, joining)
			}
		}
		p.mu.Lock()
		if joining == "" {
			p.joining = ""
		}
		p.mu.Unlock()
	}
	for i := range st.Networks {
		st.Networks[i].Saved = contains(st.Saved, st.Networks[i].SSID)
		st.Networks[i].Current = st.Link == "connected" && st.Networks[i].SSID == st.SSID
	}
	if st.Link == "connected" {
		if sig, ok := p.wpa.signal(); ok {
			st.Signal = &sig
		}
	}
}

// publish keeps st, with the lease and a failed join, and writes it to
// status.json when it changed.
func (p *wifiProvider) publish(st wifiStatus) {
	p.mu.Lock()
	if st.Link == "connected" && p.ip != "" {
		st.Link, st.IP = "online", p.ip
	}
	if st.Error == nil && p.err != "" && st.Enabled {
		msg := p.err
		st.Error = &msg
	}
	old := p.st
	p.st = st
	p.mu.Unlock()
	if reflect.DeepEqual(old, st) {
		return
	}
	if old.Link != st.Link || old.SSID != st.SSID {
		logf("wifi: %s %s", st.Link, st.SSID)
	}
	if st.Error != nil && (old.Error == nil || *old.Error != *st.Error) {
		logf("wifi: %s", *st.Error)
	}
	setStatusWiFi(&st)
}

func (p *wifiProvider) status() wifiStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

// own unblocks the interface's radio, turns IPv6 off on the interface (a
// router advertisement must not give the whole unit a route) and brings
// it up, recording each step first.
func (p *wifiProvider) own() error {
	p.mu.Lock()
	owned := p.owned
	p.mu.Unlock()
	if owned {
		return nil
	}
	var rec wifiRecord
	soft, _ := filepath.Glob(filepath.Join(p.sys, p.iface, "phy80211", "rfkill*", "soft"))
	for _, f := range soft {
		if v, _ := readSysctl(f); v == "1" {
			rec.Radio = append(rec.Radio, f)
		}
	}
	if err := writeJSON(p.record, rec, 0644); err != nil {
		return fmt.Errorf("%s: %v", p.record, err)
	}
	for _, f := range rec.Radio {
		if err := writeSysctl(f, "0"); err != nil {
			restoreWiFi(p.record)
			return fmt.Errorf("unblock the radio: %v", err)
		}
	}
	ls, err := p.linkUp(p.iface)
	rec.Link = ls
	if e := writeJSON(p.record, rec, 0644); err == nil {
		err = e
	}
	if err != nil {
		restoreWiFi(p.record)
		return err
	}
	p.mu.Lock()
	p.owned = true
	p.mu.Unlock()
	logf("wifi: %s on", p.iface)
	if p.notify != nil {
		p.notify()
	}
	return nil
}

func wifiLinkUp(iface string) (linkState, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return linkState{}, err
	}
	a := link.Attrs()
	st := linkState{Index: a.Index, Name: iface, OrigName: iface, WasUp: a.Flags&net.FlagUp != 0}
	st.IPv6, _ = readSysctl(ipv6Sysctl(iface))
	if err := disableIPv6(iface); err != nil {
		return st, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return st, fmt.Errorf("set %s up: %v", iface, err)
	}
	return st, nil
}

// release stops the supplicant and puts the radio and interface back. A
// link on the interface ends first, and the service lets go of it (DHCP's
// release still goes out) before the interface goes down.
func (p *wifiProvider) release() {
	p.mu.Lock()
	owned, l := p.owned, p.link
	p.owned, p.joining, p.link = false, "", nil
	p.mu.Unlock()
	if l != nil {
		l.end(errors.New("Wi-Fi switched off"))
		select {
		case <-l.stop:
		case <-time.After(3 * time.Second):
		}
	}
	if !owned {
		return
	}
	p.wpa.stop()
	restoreWiFi(p.record)
	logf("wifi: %s off", p.iface)
	if p.notify != nil {
		p.notify()
	}
}

// restoreWiFi puts back what a record says the provider changed.
func restoreWiFi(record string) {
	b, err := os.ReadFile(record)
	if err != nil {
		return
	}
	var rec wifiRecord
	if json.Unmarshal(b, &rec) == nil {
		if rec.Link.Index > 0 {
			rec.Link.restore()
		}
		for _, f := range rec.Radio {
			writeSysctl(f, "1")
		}
	}
	os.Remove(record)
}

// associated reports the network the supplicant is joined to.
func (p *wifiProvider) associated() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st.Link == "connected" || p.st.Link == "online" {
		return p.st.SSID, true
	}
	return "", false
}

// guarding: the provider has the interface, so the firewall guards it.
func (p *wifiProvider) guarding() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owned
}

// setOnline records the uplink's lease address on the interface ("" none).
func (p *wifiProvider) setOnline(ip string) {
	p.mu.Lock()
	changed := p.ip != ip
	p.ip = ip
	p.mu.Unlock()
	if changed {
		p.poke()
	}
}

// ---- requests (ctl.go) ----

func (p *wifiProvider) setEnabled(on bool) {
	p.mu.Lock()
	changed := p.enabled != on
	p.enabled, p.err = on, ""
	p.mu.Unlock()
	if changed {
		logf("wifi: switched %s", onOff(on))
	}
	p.poke()
}

// ready says why requests for the supplicant can't be served now.
func (p *wifiProvider) ready() error {
	p.mu.Lock()
	enabled, st := p.enabled, p.st
	p.err = ""
	p.mu.Unlock()
	switch {
	case !enabled:
		return errors.New("Wi-Fi is off")
	case st.Link == "nohw":
		return fmt.Errorf("no Wi-Fi radio (%s)", p.iface)
	case st.Link == "busy" || st.Link == "failed":
		return errors.New(*st.Error)
	case !p.wpa.isRunning():
		return errors.New("Wi-Fi is starting")
	}
	return nil
}

func (p *wifiProvider) scan() error {
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	if err := p.ready(); err != nil {
		return err
	}
	p.mu.Lock()
	p.scanUntil = time.Now().Add(scanShown)
	p.mu.Unlock()
	defer p.poke()
	return p.wpa.scan()
}

// connect joins ssid, saving it once joined. psk empty: a saved network
// as saved, or an open one.
func (p *wifiProvider) connect(ssid, psk string) error {
	if err := checkSSID(ssid); err != nil {
		return err
	}
	if psk != "" {
		if err := checkPSK(psk); err != nil {
			return err
		}
	}
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	if err := p.ready(); err != nil {
		return err
	}
	p.cancelJoin()
	open := false
	for _, n := range p.status().Networks {
		if n.SSID == ssid {
			open = !n.Secure
		}
	}
	_, had := p.wpa.saved()[ssid]
	if err := p.wpa.join(ssid, psk, open); err != nil {
		p.wpa.reload()
		return err
	}
	p.mu.Lock()
	p.joining, p.joinNew, p.joinedAt = ssid, !had, time.Now()
	p.mu.Unlock()
	logf("wifi: joining %q", ssid)
	p.poke()
	return nil
}

func (p *wifiProvider) forget(ssid string) error {
	if err := checkSSID(ssid); err != nil {
		return err
	}
	p.reqMu.Lock()
	defer p.reqMu.Unlock()
	if err := p.ready(); err != nil {
		return err
	}
	p.cancelJoin()
	logf("wifi: forgetting %q", ssid)
	defer p.poke()
	return p.wpa.forget(ssid)
}

// cancelJoin drops a join in progress, so that nothing it set up is saved.
func (p *wifiProvider) cancelJoin() {
	p.mu.Lock()
	joining := p.joining
	p.joining = ""
	p.mu.Unlock()
	if joining != "" {
		p.wpa.reload()
	}
}

func checkSSID(s string) error {
	if s == "" || len(s) > 32 {
		return errors.New("network name: 1 to 32 bytes")
	}
	return nil
}

// checkPSK: a WPA passphrase, 8 to 63 printable ASCII characters (it goes
// into wpa.conf between quotes, so no line breaks).
func checkPSK(p string) error {
	if len(p) < 8 || len(p) > 63 {
		return errors.New("password: 8 to 63 characters")
	}
	for _, c := range p {
		if c < 32 || c > 126 {
			return errors.New("password: letters, digits and symbols only")
		}
	}
	return nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func without(list []string, s string) []string {
	out := []string{}
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// ---- the link (scan.go opens it) ----

// wifiLink is the uplink on the Wi-Fi interface while the supplicant is
// joined to one network. It ends when Wi-Fi is switched off, the
// supplicant joins another network, or the network is gone for
// wifiDropAfter. Close leaves the association: the network stays a
// fallback.
type wifiLink struct {
	p       *wifiProvider
	ssid    string
	done    chan struct{}
	stop    chan struct{}
	endOnce sync.Once
	once    sync.Once
	err     error
}

func (p *wifiProvider) openLink(ssid string) *wifiLink {
	l := &wifiLink{p: p, ssid: ssid, done: make(chan struct{}), stop: make(chan struct{})}
	p.mu.Lock()
	p.link = l
	p.mu.Unlock()
	go l.watch()
	return l
}

func (l *wifiLink) end(err error) {
	l.endOnce.Do(func() {
		l.err = err
		close(l.done)
	})
}

func (l *wifiLink) watch() {
	t := time.NewTicker(wifiEvery)
	defer t.Stop()
	var lost time.Time
	for {
		select {
		case <-l.stop:
			return
		case <-l.done:
			return
		case <-t.C:
		}
		ssid, ok := l.p.associated()
		switch {
		case ok && ssid == l.ssid:
			lost = time.Time{}
		case !l.p.status().Enabled:
			l.end(errors.New("Wi-Fi switched off"))
		case ok:
			l.end(fmt.Errorf("joined %s instead", ssid))
		case lost.IsZero():
			lost = time.Now()
		case time.Since(lost) >= wifiDropAfter:
			l.end(fmt.Errorf("%s is gone", l.ssid))
		}
	}
}

func (l *wifiLink) Ifname() string        { return l.p.iface }
func (l *wifiLink) Done() <-chan struct{} { return l.done }
func (l *wifiLink) Err() error            { return l.err }
func (l *wifiLink) Close()                { l.once.Do(func() { close(l.stop) }) }
func (l *wifiLink) String() string        { return fmt.Sprintf("Wi-Fi %s on %q", l.p.iface, l.ssid) }
func (l *wifiLink) device() statusDevice  { return statusDevice{Kind: kindWiFi, Name: l.ssid} }
