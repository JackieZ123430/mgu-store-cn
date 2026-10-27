package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fastWiFi shortens the provider's timings for a test.
func fastWiFi(t *testing.T) {
	every, join, drop := wifiEvery, joinTimeout, wifiDropAfter
	wifiEvery, joinTimeout, wifiDropAfter = 50*time.Millisecond, 1500*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { wifiEvery, joinTimeout, wifiDropAfter = every, join, drop })
}

// testProvider is a provider on a fake radio (sysfs with one rfkill, soft
// blocked) and the fake wpa_supplicant (wpa_test.go) with scan file scan.
func testProvider(t *testing.T, scan string) (*wifiProvider, config, string) {
	dir := t.TempDir()
	c := defaults()
	c.RunDir, c.StateDir, c.WiFiIf = filepath.Join(dir, "run"), filepath.Join(dir, "state"), "wlant0"
	os.MkdirAll(c.RunDir, 0755)
	p := newWiFiProvider(c, os.Args[0])
	p.sys, p.proc = filepath.Join(dir, "sys"), filepath.Join(dir, "proc")
	os.MkdirAll(p.proc, 0755)
	soft := filepath.Join(p.sys, "wlant0", "phy80211", "rfkill7", "soft")
	writeFile(t, soft, "1\n")
	p.linkUp = func(string) (linkState, error) { return linkState{}, nil }
	t.Setenv(fakeWPAEnv, scan)
	return p, c, soft
}

func waitWiFi(t *testing.T, p *wifiProvider, what string, ok func(wifiStatus) bool) wifiStatus {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if st := p.status(); ok(st) {
			return st
		}
	}
	b, _ := json.Marshal(p.status())
	t.Fatalf("%s: %s", what, b)
	return wifiStatus{}
}

func TestWiFiProvider(t *testing.T) {
	fastWiFi(t)
	scan := filepath.Join(t.TempDir(), "scan")
	home := "0a:3c:ff:5b:bc:a7\t2437\t-40\t[WPA2-PSK-CCMP][ESS]\tHome\trightpass1"
	cafe := "40:d6:3c:68:0c:b7\t2462\t-60\t[ESS]\tCafe\t"
	other := "bb:bb:bb:bb:bb:bb\t2412\t-70\t[WPA2-PSK-CCMP][ESS]\tOther\tx12345678"
	writeScan(t, scan, home, cafe, other)
	p, c, soft := testProvider(t, scan)
	statusInit(c.statusFile())
	defer statusInit("")
	setStatus(nil, stateSearching, "")
	// every log line, and the supplicant's output, for the psk check at the end
	logFile := filepath.Join(t.TempDir(), "log")
	out, _ := os.Create(logFile)
	stdout := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = stdout }()

	p.start()
	stopped := false
	defer func() {
		if !stopped {
			p.stop()
		}
	}()
	waitWiFi(t, p, "off at start", func(st wifiStatus) bool { return st.Link == "off" && !st.Enabled })
	if err := p.scan(); err == nil || err.Error() != "Wi-Fi is off" {
		t.Fatalf("scan while off: %v", err)
	}
	if v, _ := readSysctl(soft); v != "1" {
		t.Fatal("radio unblocked while off")
	}

	p.setEnabled(true)
	waitWiFi(t, p, "idle with 3 networks", func(st wifiStatus) bool { return st.Link == "idle" && len(st.Networks) == 3 })
	if v, _ := readSysctl(soft); v != "0" || !p.guarding() {
		t.Fatalf("radio %q guarding %v", v, p.guarding())
	}
	if err := p.scan(); err != nil {
		t.Fatal(err)
	}
	waitWiFi(t, p, "scanning", func(st wifiStatus) bool { return st.Scanning })
	for psk, want := range map[string]string{"": "needs a password", "short": "8 to 63"} {
		if err := p.connect("Other", psk); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("connect psk %q: %v", psk, err)
		}
	}

	// A wrong password: connecting, not shown as saved, then failed and dropped.
	if err := p.connect("Home", "wrongpass9"); err != nil {
		t.Fatal(err)
	}
	waitWiFi(t, p, "joining Home", func(st wifiStatus) bool {
		return st.Link == "connecting" && st.SSID == "Home" && len(st.Saved) == 0 && !st.Networks[0].Saved
	})
	waitWiFi(t, p, "join failed", func(st wifiStatus) bool {
		return st.Error != nil && strings.Contains(*st.Error, "could not join Home") && len(st.Saved) == 0
	})
	if b, _ := os.ReadFile(c.wpaConf()); strings.Contains(string(b), "Home") {
		t.Fatalf("a failed join was saved:\n%s", b)
	}

	// The right one: joined, saved, the folder root's only.
	if err := p.connect("Home", "rightpass1"); err != nil {
		t.Fatal(err)
	}
	st := waitWiFi(t, p, "joined Home", func(st wifiStatus) bool {
		return st.Link == "connected" && st.SSID == "Home" && reflect.DeepEqual(st.Saved, []string{"Home"})
	})
	if st.Error != nil || st.Signal == nil || *st.Signal != -45 || !st.Networks[0].Current || !st.Networks[0].Saved {
		b, _ := json.Marshal(st)
		t.Fatalf("joined: %s", b)
	}
	if b, _ := os.ReadFile(c.wpaConf()); !strings.Contains(string(b), `ssid="Home"`) {
		t.Fatalf("Home not saved:\n%s", b)
	}
	if fi, err := os.Stat(filepath.Dir(c.wpaConf())); err != nil || fi.Mode().Perm() != 0700 {
		t.Fatalf("wpa.conf's folder: %v %v", fi, err)
	}
	if ssid, ok := p.associated(); !ok || ssid != "Home" {
		t.Fatalf("associated %q %v", ssid, ok)
	}

	// A link on Home ends when the supplicant joins another network.
	l := p.openLink("Home")
	if l.Ifname() != "wlant0" || l.device() != (statusDevice{Kind: kindWiFi, Name: "Home"}) {
		t.Fatalf("link %s %+v", l.Ifname(), l.device())
	}
	if err := p.connect("Cafe", ""); err != nil {
		t.Fatal(err)
	}
	waitWiFi(t, p, "joined Cafe", func(st wifiStatus) bool { return st.Link == "connected" && st.SSID == "Cafe" })
	waitDone(t, l, "joined Cafe instead")

	// ... and when its network is gone.
	l = p.openLink("Cafe")
	writeScan(t, scan, other)
	waitDone(t, l, "Cafe is gone")
	writeScan(t, scan, home, cafe, other)
	if err := p.forget("Cafe"); err != nil {
		t.Fatal(err)
	}
	if err := p.forget("Nope"); err == nil {
		t.Fatal("forgot a network that is not saved")
	}
	waitWiFi(t, p, "back on Home", func(st wifiStatus) bool {
		return st.Link == "connected" && st.SSID == "Home" && reflect.DeepEqual(st.Saved, []string{"Home"})
	})

	// The uplink's lease on the interface makes it online.
	p.setOnline("172.20.10.2")
	waitWiFi(t, p, "online", func(st wifiStatus) bool { return st.Link == "online" && st.IP == "172.20.10.2" })
	if b, _ := os.ReadFile(c.statusFile()); !strings.Contains(string(b), `"link": "online"`) ||
		!strings.Contains(string(b), `"ssid": "Home"`) {
		t.Fatalf("status.json: %s", b)
	}

	p.setEnabled(false)
	waitWiFi(t, p, "off again", func(st wifiStatus) bool { return st.Link == "off" })
	if v, _ := readSysctl(soft); v != "1" || p.wpa.isRunning() || p.guarding() || exists(p.record) {
		t.Fatalf("after off: radio %q, supplicant running %v, guarding %v", v, p.wpa.isRunning(), p.guarding())
	}
	p.stop()
	stopped = true
	out.Close()
	if b, _ := os.ReadFile(logFile); strings.Contains(string(b), "rightpass1") || strings.Contains(string(b), "wrongpass9") {
		t.Fatalf("a psk reached the log:\n%s", b)
	}
}

func waitDone(t *testing.T, l *wifiLink, why string) {
	t.Helper()
	select {
	case <-l.Done():
		if l.Err() == nil || !strings.Contains(l.Err().Error(), why) {
			t.Fatalf("link ended: %v, want %q", l.Err(), why)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("link did not end (%s)", why)
	}
	l.Close()
}

// Someone else's wpa_supplicant on the interface: the provider leaves the
// radio alone until it is gone.
func TestWiFiBusy(t *testing.T) {
	fastWiFi(t)
	scan := filepath.Join(t.TempDir(), "scan")
	writeScan(t, scan)
	p, _, soft := testProvider(t, scan)
	other := filepath.Join(p.proc, "77", "cmdline")
	writeFile(t, other, "/usr/sbin/wpa_supplicant_nxp\x00-iwlant0\x00")
	p.setEnabled(true)
	p.start()
	defer p.stop()
	waitWiFi(t, p, "busy", func(st wifiStatus) bool {
		return st.Link == "busy" && st.Error != nil && strings.Contains(*st.Error, "another wpa_supplicant")
	})
	if v, _ := readSysctl(soft); v != "1" || p.guarding() {
		t.Fatal("took a radio someone else uses")
	}
	if err := p.scan(); err == nil {
		t.Fatal("scan while busy")
	}
	os.RemoveAll(filepath.Dir(other))
	waitWiFi(t, p, "idle once free", func(st wifiStatus) bool { return st.Link == "idle" })
}

// The preferred kind goes first, the others keep their order; Wi-Fi is a
// candidate only while joined.
func TestPreferredOrder(t *testing.T) {
	c := defaults()
	u := newScanUplink(c)
	order := func() []string {
		ks := append([]string(nil), autoKinds...)
		sort.SliceStable(ks, func(i, j int) bool { return u.rank(ks[i]) < u.rank(ks[j]) })
		return ks
	}
	if got := order(); !reflect.DeepEqual(got, []string{"usb", "usbecm", "wifi", "rndis", "ncm", "iphone"}) {
		t.Fatalf("default order %v", got)
	}
	u.setPreferred("iphone")
	if got := order(); !reflect.DeepEqual(got, []string{"iphone", "usb", "usbecm", "wifi", "rndis", "ncm"}) {
		t.Fatalf("iphone preferred: %v", got)
	}
	c.Preferred = "wifi"
	u = newScanUplink(c)
	if got := order(); got[0] != "wifi" {
		t.Fatalf("wifi preferred: %v", got)
	}

	p := newWiFiProvider(c, "")
	u.wifi = p
	p.st = wifiStatus{Enabled: true, Link: "connected", SSID: "Home"}
	found := false
	for _, cand := range u.candidates() {
		found = found || cand.kind == kindWiFi && cand.netdev == wifiIfname && cand.dev.Name == "Home"
	}
	if !found {
		t.Fatalf("no Wi-Fi candidate: %+v", u.candidates())
	}
	p.st.Link = "idle"
	for _, cand := range u.candidates() {
		if cand.kind == kindWiFi {
			t.Fatal("a Wi-Fi candidate while not joined")
		}
	}
}

// The firewall guards the uplink interface, the link's, and the Wi-Fi
// interface while the provider has it.
func TestGuarded(t *testing.T) {
	c := defaults()
	p := newWiFiProvider(c, "")
	s := &service{cfg: c, up: newScanUplink(c), wifi: p}
	if got := s.guarded(); !reflect.DeepEqual(got, []string{Ifname}) {
		t.Fatalf("wifi off: %v", got)
	}
	p.owned = true
	if got := s.guarded(); !reflect.DeepEqual(got, []string{Ifname, wifiIfname}) {
		t.Fatalf("wifi on: %v", got)
	}
	p.owned = false
	s.link = p.openLink("Home")
	defer s.link.Close()
	if got := s.guarded(); !reflect.DeepEqual(got, []string{Ifname, wifiIfname}) {
		t.Fatalf("on a Wi-Fi link: %v", got)
	}
	s = &service{cfg: c, up: &netdevUplink{name: "eth1"}}
	if got := s.guarded(); !reflect.DeepEqual(got, []string{"eth1"}) {
		t.Fatalf("netdev: %v", got)
	}
}

func TestParseUnitAccepts(t *testing.T) {
	got := parseUnitAccepts("table ip filter {\n\tchain output { # handle 3\n" +
		"\t\ttype filter hook output priority 0; policy accept;\n" +
		"\t\toifname \"wlan1\" accept comment \"mgu-uplink\" # handle 41\n" +
		"\t\toifname \"vethuplink\" accept comment \"mgu-uplink\" # handle 40\n" +
		"\t\tjump highpriooutput # handle 12\n" +
		"\t\tmeta skuid 12205 accept comment \"other\" # handle 9\n\t}\n}\n")
	if !reflect.DeepEqual(got, map[string]string{"41": "wlan1", "40": "vethuplink"}) {
		t.Fatalf("%v", got)
	}
}

// Requests change settings_conf line by line: comments and other keys
// stay, and loadConfig reads the result after local_conf.
func TestSettings(t *testing.T) {
	if c, err := loadConfig("mgu-uplink.conf"); err != nil || c.WiFi || c.Preferred != "" ||
		c.Settings != "/var/opt/mgu-uplink/mgu-uplink.settings.conf" {
		t.Fatalf("shipped conf: %+v %v", c, err)
	}
	dir := t.TempDir()
	p, local, settings := filepath.Join(dir, "c.conf"), filepath.Join(dir, "local.conf"), filepath.Join(dir, "settings.conf")
	writeFile(t, p, "local_conf = "+local+"\nsettings_conf = "+settings+"\napps = store\n")
	writeFile(t, local, "preferred = iphone\nwifi = off\n")
	c, err := loadConfig(p)
	if err != nil || c.WiFi || c.Preferred != "iphone" {
		t.Fatalf("local only: %+v %v", c, err)
	}
	writeFile(t, settings, "# mine\nuplink = auto # kept\npreferred = usb\npreferred = rndis")
	steps := []struct{ key, val, want string }{
		{"preferred", "wifi", "# mine\nuplink = auto # kept\npreferred = wifi\n"},
		{"wifi", "on", "# mine\nuplink = auto # kept\npreferred = wifi\nwifi = on\n"},
	}
	for _, s := range steps {
		if err := setSetting(settings, s.key, s.val); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(settings); string(b) != s.want {
			t.Fatalf("%s = %s:\n%q", s.key, s.val, b)
		}
	}
	if c, err = loadConfig(p); err != nil || !c.WiFi || c.Preferred != "wifi" {
		t.Fatalf("read back: %+v %v", c, err)
	}
	setSetting(settings, "preferred", "")
	if b, _ := os.ReadFile(settings); strings.Contains(string(b), "preferred") {
		t.Fatalf("preferred kept:\n%s", b)
	}
	if c, err = loadConfig(p); err != nil || c.Preferred != "iphone" {
		t.Fatalf("local's preferred after the setting went: %+v %v", c, err)
	}
	fresh := filepath.Join(dir, "new.conf")
	if err := setSetting(fresh, "wifi", "off"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(fresh); string(b) != "wifi = off\n" {
		t.Fatalf("new file %q", b)
	}
	if err := setSetting("", "wifi", "on"); err == nil {
		t.Fatal("no settings_conf accepted")
	}
	for _, bad := range []string{"preferred = ppp\n", "wifi = yes\n", "settings_conf = /elsewhere\n", "local_conf = /x\n"} {
		writeFile(t, settings, bad)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestControlRequests(t *testing.T) {
	dir := t.TempDir()
	c := defaults()
	c.RunDir, c.Settings = dir, filepath.Join(dir, "settings.conf")
	writeFile(t, c.Settings, "# the Store's\npreferred = usb\n")
	statusInit(c.statusFile())
	defer statusInit("")
	setStatus(nil, stateSearching, "")
	su := newScanUplink(c)
	p := newWiFiProvider(c, filepath.Join(dir, "none")) // never started
	s := &service{cfg: c, up: su, wifi: p}
	ln, err := listenCtl(c.ctlSocket(), s.request)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if fi, err := os.Stat(c.ctlSocket()); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("ctl.sock: %v %v", fi, err)
	}
	ask := func(req string) ctlReply {
		t.Helper()
		conn, err := net.Dial("unix", c.ctlSocket())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte(req + "\n"))
		var r ctlReply
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("%s: reply %q", req, line)
		}
		return r
	}
	for _, tc := range []struct{ req, err string }{
		{`{"op":"prefer","kind":"wifi"}`, ""},
		{`{"op":"prefer","kind":"ppp"}`, "none of"},
		{`{"op":"prefer"}`, "needs a kind"},
		{`{"op":"wifi-enable"}`, "on: true or false"},
		{`{"op":"wifi-enable","on":true}`, ""},
		{`{"op":"wifi-scan"}`, "Wi-Fi is starting"},
		{`{"op":"wifi-connect","ssid":"Home","psk":"short"}`, "8 to 63"},
		{`{"op":"wifi-forget","ssid":""}`, "1 to 32"},
		{`{"op":"reboot"}`, "unknown op"},
		{`not json`, "bad request"},
		{`{"op":"` + strings.Repeat("x", ctlMaxRequest) + `"}`, "bad request"},
	} {
		r := ask(tc.req)
		if tc.err == "" && (!r.OK || r.Error != "") || tc.err != "" && (r.OK || !strings.Contains(r.Error, tc.err)) {
			t.Errorf("%.60s: %+v, want error %q", tc.req, r, tc.err)
		}
	}
	if b, _ := os.ReadFile(c.Settings); string(b) != "# the Store's\npreferred = wifi\nwifi = on\n" {
		t.Fatalf("settings_conf:\n%s", b)
	}
	if u := su.rank(kindWiFi); u != 0 || !p.enabled {
		t.Fatalf("rank %d enabled %v", u, p.enabled)
	}
	if b, _ := os.ReadFile(c.statusFile()); !strings.Contains(string(b), `"preferred": "wifi"`) {
		t.Fatalf("status.json: %s", b)
	}
	if r := ask(`{"op":"prefer","kind":""}`); !r.OK {
		t.Fatal(r.Error)
	}
	if b, _ := os.ReadFile(c.statusFile()); strings.Contains(string(b), `"preferred"`) {
		t.Fatalf("status.json still names a preferred kind: %s", b)
	}

	// With a named interface (netdev:) there is no Wi-Fi and no order.
	s = &service{cfg: c, up: &netdevUplink{name: "eth1"}}
	kind := "wifi"
	for _, r := range []ctlRequest{{Op: "wifi-scan"}, {Op: "wifi-connect", SSID: "x"}, {Op: "prefer", Kind: &kind}} {
		if err := s.request(r); err == nil {
			t.Errorf("%s accepted with uplink netdev:", r.Op)
		}
	}
}

// status.json carries the preferred kind and the Wi-Fi state only while
// there are any.
func TestStatusWiFi(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	statusInit(path)
	defer statusInit("")
	setStatusWiFi(&wifiStatus{Link: "off", Networks: []wifiNetwork{}, Saved: []string{}})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("written before a state")
	}
	setStatus(&statusDevice{Kind: kindWiFi, Name: "Home"}, stateOnline, "")
	setStatusPreferred("wifi")
	b, _ := os.ReadFile(path)
	var st uplinkStatus
	if err := json.Unmarshal(b, &st); err != nil || st.Preferred != "wifi" || st.WiFi == nil || st.WiFi.Link != "off" ||
		st.Device == nil || st.Device.Kind != "wifi" || !strings.Contains(string(b), `"error": null`) {
		t.Fatalf("%s", b)
	}
}
