package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// fakeWPAEnv makes the test binary, started as "<binary> -i <iface> ... -c
// <conf>", a fake wpa_supplicant (fakeWPA); its value is the scan file.
const fakeWPAEnv = "MGU_UPLINK_FAKE_WPA"

func TestMain(m *testing.M) {
	if scan := os.Getenv(fakeWPAEnv); scan != "" && len(os.Args) > 1 && os.Args[1] == "-i" {
		os.Exit(fakeWPA(scan, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestParseScan(t *testing.T) {
	r := "bssid / frequency / signal level / flags / ssid\n" +
		"0a:3c:ff:5b:bc:a7\t2437\t-36\t[WPA2-PSK-CCMP][ESS]\tiPhone - Mario\n" +
		"40:d6:3c:68:0c:b7\t2462\t-31\t[WPA2-PSK-CCMP][ESS]\tMHD ENET 7777\n" +
		"aa:aa:aa:aa:aa:aa\t2437\t-80\t[WPA2-PSK-CCMP][ESS]\tiPhone - Mario\n" +
		"bb:bb:bb:bb:bb:bb\t2412\t-70\t[ESS]\tCafe\\x20Free\n" +
		"cc:cc:cc:cc:cc:cc\t2412\t-50\t[ESS]\t\n"
	n := parseScan(r)
	if len(n) != 3 || n[0].SSID != "MHD ENET 7777" || n[1].SSID != "iPhone - Mario" || n[1].Signal != -36 {
		t.Fatalf("scan: %+v", n)
	}
	if n[2].SSID != "Cafe Free" || n[2].Secure || !n[1].Secure {
		t.Fatalf("open / escaped: %+v", n[2])
	}
	var many strings.Builder
	many.WriteString("bssid / frequency / signal level / flags / ssid\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&many, "aa:aa:aa:aa:aa:%02x\t2412\t-%d\t[ESS]\tnet%d\n", i, 40+i, i)
	}
	if n := parseScan(many.String()); len(n) != maxNetworks || n[0].SSID != "net0" {
		t.Fatalf("%d networks, first %+v", len(n), n[0])
	}
}

func TestParseSaved(t *testing.T) {
	m := parseSaved("network id / ssid / bssid / flags\n0\tiPhone - Mario\tany\t[CURRENT]\n3\tHome\tany\t[DISABLED]\n")
	if m["iPhone - Mario"] != 0 || m["Home"] != 3 || len(m) != 2 {
		t.Fatalf("saved: %v", m)
	}
	if kv := parseKV("wpa_state=COMPLETED\nssid=a=b\n"); kv["wpa_state"] != "COMPLETED" || kv["ssid"] != "a=b" {
		t.Fatalf("kv: %v", kv)
	}
	if s := unescape(`a\\b\"c\x41`); s != `a\b"cA` {
		t.Fatalf("unescape: %q", s)
	}
}

func TestWiFiChecks(t *testing.T) {
	if checkPSK("1234567") == nil || checkPSK("12345678") != nil || checkPSK("bad\npassword") == nil ||
		checkPSK(strings.Repeat("a", 64)) == nil || checkPSK("päss-word") == nil {
		t.Fatal("psk checks")
	}
	if checkSSID("") == nil || checkSSID(strings.Repeat("a", 33)) == nil || checkSSID("ok") != nil {
		t.Fatal("ssid checks")
	}
}

// Only a wpa_supplicant with our config is ours; one on the interface with
// another config is someone else's; one on another interface is not seen.
func TestSupplicants(t *testing.T) {
	proc := t.TempDir()
	conf := "/var/opt/mgu-uplink/wifi/wpa.conf"
	for pid, args := range map[string][]string{
		"101": {"/var/opt/mgu-uplink/current/wpa_supplicant", "-i", "wlan1", "-D", "nl80211", "-c", conf},
		"102": {"/usr/sbin/wpa_supplicant_nxp", "-iwlan1", "-c/etc/wpa.conf"},
		"103": {"/usr/sbin/wpa_supplicant_nxp", "-iwfd0", "-Dnl80211"},
		"104": {"/bin/sleep", "-i", "wlan1"},
	} {
		writeFile(t, filepath.Join(proc, pid, "cmdline"), strings.Join(args, "\x00")+"\x00")
	}
	got := supplicants(proc, "wlan1", conf)
	if len(got) != 2 || !got[101] || got[102] {
		t.Fatalf("supplicants %v", got)
	}
}

// ---- a fake wpa_supplicant ----

// The scan file: one access point per line, tab-separated: bssid,
// frequency, signal, flags, ssid and the password it takes ("" open).
func writeScan(t *testing.T, path string, rows ...string) {
	t.Helper()
	writeFile(t, path, strings.Join(rows, "\n")+"\n")
}

type fakeNet struct {
	ssid, psk, keyMgmt string
	prio               int
	off                bool
}

// fakeWPA serves the control commands wpa.go sends, joining the enabled
// network of the highest priority whose password matches an access point
// of the scan file.
func fakeWPA(scanFile string, args []string) int {
	var iface, conf string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-i":
			iface = args[i+1]
		case "-c":
			conf = args[i+1]
		}
	}
	ctrl, nets, next := "", map[int]*fakeNet{}, 0
	load := func() {
		b, _ := os.ReadFile(conf)
		nets, next = map[int]*fakeNet{}, 0
		var n *fakeNet
		for _, l := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
			switch {
			case k == "ctrl_interface":
				ctrl = v
			case k == "network":
				n = &fakeNet{}
				nets[next] = n
				next++
			case n != nil && k == "ssid":
				n.ssid = strings.Trim(v, `"`)
			case n != nil && k == "psk":
				n.psk = strings.Trim(v, `"`)
			case n != nil && k == "key_mgmt":
				n.keyMgmt = v
			case n != nil && k == "priority":
				n.prio, _ = strconv.Atoi(v)
			}
		}
	}
	scanRows := func() [][]string {
		b, _ := os.ReadFile(scanFile)
		var rows [][]string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if f := strings.Split(l, "\t"); len(f) == 6 {
				rows = append(rows, f)
			}
		}
		return rows
	}
	state, cur := "INACTIVE", -1
	try := func() {
		best := -1
		for id, n := range nets {
			for _, r := range scanRows() {
				if !n.off && r[4] == n.ssid && (r[5] == "" && n.keyMgmt == "NONE" || r[5] != "" && r[5] == n.psk) &&
					(best < 0 || n.prio > nets[best].prio) {
					best = id
				}
			}
		}
		state, cur = "INACTIVE", best
		for _, n := range nets {
			if !n.off {
				state = "SCANNING"
			}
		}
		if best >= 0 {
			state = "COMPLETED"
		}
	}
	load()
	try()
	os.MkdirAll(ctrl, 0770)
	srv, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(ctrl, iface), Net: "unixgram"})
	if err != nil {
		return 1
	}
	buf := make([]byte, 4096)
	for {
		n, from, err := srv.ReadFromUnix(buf)
		if err != nil {
			return 0
		}
		f := strings.SplitN(string(buf[:n]), " ", 4)
		id := -1
		if len(f) > 1 {
			if id, err = strconv.Atoi(f[1]); err != nil {
				id = -1
			}
		}
		reply := "OK\n"
		switch f[0] {
		case "STATUS":
			if cur >= 0 && nets[cur] != nil {
				still := false
				for _, r := range scanRows() {
					still = still || r[4] == nets[cur].ssid
				}
				if !still {
					try()
				}
			}
			reply = "wpa_state=" + state + "\n"
			if state == "COMPLETED" {
				reply += "ssid=" + nets[cur].ssid + "\n"
			}
		case "SIGNAL_POLL":
			reply = "FAIL\n"
			if state == "COMPLETED" {
				reply = "RSSI=-45\nLINKSPEED=72\n"
			}
		case "SCAN_RESULTS":
			reply = "bssid / frequency / signal level / flags / ssid\n"
			for _, r := range scanRows() {
				reply += strings.Join(r[:5], "\t") + "\n"
			}
		case "LIST_NETWORKS":
			reply = "network id / ssid / bssid / flags\n"
			var ids []int
			for i := range nets {
				ids = append(ids, i)
			}
			sort.Ints(ids)
			for _, i := range ids {
				reply += fmt.Sprintf("%d\t%s\tany\t\n", i, nets[i].ssid)
			}
		case "ADD_NETWORK":
			nets[next] = &fakeNet{}
			reply = fmt.Sprintf("%d\n", next)
			next++
		case "SET_NETWORK":
			nn := nets[id]
			if nn == nil || len(f) < 4 {
				reply = "FAIL\n"
				break
			}
			switch f[2] {
			case "ssid":
				b, _ := hex.DecodeString(f[3])
				nn.ssid = string(b)
			case "psk":
				nn.psk = strings.Trim(f[3], `"`)
			case "key_mgmt":
				nn.keyMgmt = f[3]
			case "priority":
				nn.prio, _ = strconv.Atoi(f[3])
			}
		case "SELECT_NETWORK":
			for i, nn := range nets {
				nn.off = i != id
			}
			try()
		case "ENABLE_NETWORK":
			for _, nn := range nets {
				nn.off = false
			}
		case "REMOVE_NETWORK":
			delete(nets, id)
			try()
		case "RECONFIGURE":
			load()
			try()
		case "SAVE_CONFIG":
			out := fmt.Sprintf("ctrl_interface=%s\nupdate_config=1\nap_scan=1\n", ctrl)
			for i := 0; i < next; i++ {
				if nn := nets[i]; nn != nil {
					out += fmt.Sprintf("\nnetwork={\n\tssid=\"%s\"\n\tpsk=\"%s\"\n\tkey_mgmt=%s\n\tpriority=%d\n}\n",
						nn.ssid, nn.psk, nn.keyMgmt, nn.prio)
				}
			}
			os.WriteFile(conf, []byte(out), 0644) // the real one ignores the file's mode too
		case "SCAN":
		default:
			reply = "UNKNOWN COMMAND\n"
		}
		srv.WriteToUnix([]byte(reply), from)
	}
}
