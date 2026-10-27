package main

// The Wi-Fi provider's wpa_supplicant (wifi.go): upstream 2.10 with
// wpa/wpa-mgu-sme-connect.patch, built by `make target` and installed next
// to mgu-uplink. It runs on the Wi-Fi interface only, with the saved
// networks in wpa.conf (update_config: SAVE_CONFIG writes them), and all
// talk goes over its control socket. Any other wpa_supplicant on the unit
// (the stock one CarPlay uses) is never touched.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	restartSupplicant = 5 * time.Second // after it exited by itself
	maxNetworks       = 20              // rows of the scan list
)

type supplicant struct {
	bin   string // the program
	iface string
	conf  string // saved networks, in a folder only root can read
	ctrl  string // its control socket folder (ctrl_interface)
	local string // our end of the control socket

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	crashAt time.Time
	conn    *net.UnixConn
}

func (s *supplicant) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running()
}

func (s *supplicant) running() bool {
	if s.cmd == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// ensure starts the supplicant unless it runs; after it exited by itself,
// at most every restartSupplicant.
func (s *supplicant) ensure(proc string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return nil
	}
	if s.cmd != nil {
		s.cmd = nil
		s.crashAt = time.Now()
	}
	if time.Since(s.crashAt) < restartSupplicant {
		return fmt.Errorf("wpa_supplicant ended; starting it again")
	}
	s.closeConn()
	killOwnSupplicants(proc, s.conf)
	if _, err := os.Stat(s.bin); err != nil {
		return fmt.Errorf("wpa_supplicant: %v", err)
	}
	// wpa_supplicant writes the file with the default mode: the folder
	// keeps the passwords root's.
	dir := filepath.Dir(s.conf)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	if _, err := os.Stat(s.conf); os.IsNotExist(err) {
		head := fmt.Sprintf("ctrl_interface=%s\nupdate_config=1\nap_scan=1\n", s.ctrl)
		if err := os.WriteFile(s.conf, []byte(head), 0600); err != nil {
			return err
		}
	}
	os.RemoveAll(s.ctrl) // a killed run's socket would block the start
	cmd := exec.Command(s.bin, "-i", s.iface, "-D", "nl80211", "-c", s.conf)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start wpa_supplicant: %v", err)
	}
	s.cmd, s.exited = cmd, make(chan struct{})
	go func(c *exec.Cmd, done chan struct{}) {
		err := c.Wait()
		logf("wpa_supplicant on %s ended: %v", s.iface, err)
		close(done)
	}(cmd, s.exited)
	logf("wpa_supplicant started on %s", s.iface)
	return nil
}

// stop ends the supplicant; the next ensure starts it at once.
func (s *supplicant) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeConn()
	if s.running() {
		s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.exited:
		case <-time.After(3 * time.Second):
			s.cmd.Process.Kill()
			<-s.exited
		}
	}
	s.cmd, s.crashAt = nil, time.Time{}
}

func (s *supplicant) closeConn() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
		os.Remove(s.local)
	}
}

// request sends one control command and returns its reply. The socket is a
// datagram socket; ours is bound so that the supplicant can answer.
func (s *supplicant) request(cmd string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running() {
		return "", fmt.Errorf("wpa_supplicant is not running")
	}
	if s.conn == nil {
		os.Remove(s.local)
		c, err := net.DialUnix("unixgram", &net.UnixAddr{Name: s.local, Net: "unixgram"},
			&net.UnixAddr{Name: filepath.Join(s.ctrl, s.iface), Net: "unixgram"})
		if err != nil {
			return "", err
		}
		s.conn = c
	}
	s.conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := s.conn.Write([]byte(cmd)); err != nil {
		s.closeConn()
		return "", err
	}
	buf := make([]byte, 16384)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			s.closeConn()
			return "", err
		}
		if n > 0 && buf[0] == '<' { // an event; we never ATTACH
			continue
		}
		return string(buf[:n]), nil
	}
}

// ok sends cmd and expects OK. An error names the command only: a psk
// never reaches a message.
func (s *supplicant) ok(cmd string) error {
	r, err := s.request(cmd)
	if err != nil {
		return err
	}
	if strings.TrimSpace(r) != "OK" {
		return fmt.Errorf("wpa_supplicant: %s: %s", strings.Fields(cmd)[0], strings.TrimSpace(r))
	}
	return nil
}

func (s *supplicant) status() map[string]string {
	r, err := s.request("STATUS")
	if err != nil {
		return map[string]string{}
	}
	return parseKV(r)
}

func (s *supplicant) signal() (int, bool) {
	r, err := s.request("SIGNAL_POLL")
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(parseKV(r)["RSSI"])
	return v, err == nil
}

func (s *supplicant) scan() error {
	r, err := s.request("SCAN")
	if err != nil {
		return err
	}
	if t := strings.TrimSpace(r); t != "OK" && t != "FAIL-BUSY" { // busy: it is scanning anyway
		return fmt.Errorf("scan: %s", t)
	}
	return nil
}

func (s *supplicant) saved() map[string]int {
	if r, err := s.request("LIST_NETWORKS"); err == nil {
		return parseSaved(r)
	}
	return map[string]int{}
}

// networks is the newest scan, marked with the saved networks, and the
// saved names.
func (s *supplicant) networks() ([]wifiNetwork, []string) {
	saved := s.saved()
	names := make([]string, 0, len(saved))
	for n := range saved {
		names = append(names, n)
	}
	sort.Strings(names)
	nets := []wifiNetwork{}
	if r, err := s.request("SCAN_RESULTS"); err == nil {
		nets = parseScan(r)
	}
	for i := range nets {
		_, nets[i].Saved = saved[nets[i].SSID]
	}
	return nets, names
}

// join adds the network (or updates a saved one's password), gives it the
// highest priority and selects it, which keeps the others off until
// joined or reloaded. Nothing is saved yet: joined saves, reload drops it.
func (s *supplicant) join(ssid, psk string, open bool) error {
	id, have := s.saved()[ssid]
	if !have {
		if psk == "" && !open {
			return fmt.Errorf("%s needs a password", ssid)
		}
		r, err := s.request("ADD_NETWORK")
		if err != nil {
			return err
		}
		if id, err = strconv.Atoi(strings.TrimSpace(r)); err != nil {
			return fmt.Errorf("wpa_supplicant: ADD_NETWORK: %s", strings.TrimSpace(r))
		}
		// hex: any bytes; scan_ssid: hidden and sleeping hotspots answer a directed probe
		for _, c := range []string{
			fmt.Sprintf("SET_NETWORK %d ssid %s", id, hex.EncodeToString([]byte(ssid))),
			fmt.Sprintf("SET_NETWORK %d scan_ssid 1", id),
		} {
			if err := s.ok(c); err != nil {
				return err
			}
		}
	}
	var cmds []string
	switch {
	case psk != "":
		cmds = []string{
			fmt.Sprintf("SET_NETWORK %d key_mgmt WPA-PSK WPA-PSK-SHA256 SAE", id),
			fmt.Sprintf("SET_NETWORK %d ieee80211w 1", id),
			fmt.Sprintf("SET_NETWORK %d psk \"%s\"", id, psk),
		}
	case !have:
		cmds = []string{fmt.Sprintf("SET_NETWORK %d key_mgmt NONE", id)}
	}
	// the newest choice first among the saved ones
	cmds = append(cmds, fmt.Sprintf("SET_NETWORK %d priority %d", id, time.Now().Unix()/60),
		fmt.Sprintf("SELECT_NETWORK %d", id))
	for _, c := range cmds {
		if err := s.ok(c); err != nil {
			return err
		}
	}
	return nil
}

// joined keeps what join set up: every saved network on again, all saved.
func (s *supplicant) joined() error {
	if err := s.ok("ENABLE_NETWORK all"); err != nil {
		return err
	}
	return s.ok("SAVE_CONFIG")
}

// reload drops what join set up: the saved networks as saved.
func (s *supplicant) reload() error { return s.ok("RECONFIGURE") }

func (s *supplicant) forget(ssid string) error {
	id, ok := s.saved()[ssid]
	if !ok {
		return fmt.Errorf("%s is not saved", ssid)
	}
	if err := s.ok(fmt.Sprintf("REMOVE_NETWORK %d", id)); err != nil {
		return err
	}
	return s.ok("SAVE_CONFIG")
}

// ---- control replies ----

func parseKV(r string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(r, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

// parseSaved reads LIST_NETWORKS: a "network id / ssid / bssid / flags"
// header, then id, ssid, bssid and flags, tab-separated.
func parseSaved(r string) map[string]int {
	m := map[string]int{}
	for i, l := range strings.Split(r, "\n") {
		f := strings.Split(l, "\t")
		if i == 0 || len(f) < 2 {
			continue
		}
		if id, err := strconv.Atoi(f[0]); err == nil {
			m[unescape(f[1])] = id
		}
	}
	return m
}

// parseScan reads SCAN_RESULTS: a "bssid / frequency / signal level /
// flags / ssid" header, then tab-separated rows. One row per name, the
// strongest; hidden ones left out; strongest first; at most maxNetworks.
func parseScan(r string) []wifiNetwork {
	best := map[string]wifiNetwork{}
	for i, l := range strings.Split(r, "\n") {
		f := strings.Split(l, "\t")
		if i == 0 || len(f) < 5 {
			continue
		}
		ssid := unescape(f[4])
		sig, err := strconv.Atoi(f[2])
		if ssid == "" || err != nil {
			continue
		}
		n := wifiNetwork{SSID: ssid, Signal: sig,
			Secure: strings.Contains(f[3], "WPA") || strings.Contains(f[3], "RSN") || strings.Contains(f[3], "WEP")}
		if old, ok := best[ssid]; !ok || old.Signal < sig {
			best[ssid] = n
		}
	}
	out := make([]wifiNetwork, 0, len(best))
	for _, n := range best {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Signal != out[j].Signal {
			return out[i].Signal > out[j].Signal
		}
		return out[i].SSID < out[j].SSID
	})
	if len(out) > maxNetworks {
		out = out[:maxNetworks]
	}
	return out
}

// unescape undoes wpa_supplicant's printf escaping of names (\xNN, \\, \").
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'x':
				if i+3 < len(s) {
					if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
						b.WriteByte(byte(v))
						i += 3
						continue
					}
				}
			case '\\', '"':
				b.WriteByte(s[i+1])
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- other processes ----

// supplicants lists the wpa_supplicant processes under proc (/proc) that
// run on iface: pid -> whether its config is conf (ours).
func supplicants(proc, iface, conf string) map[int]bool {
	out := map[int]bool{}
	dirs, _ := os.ReadDir(proc)
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, d.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if !strings.Contains(filepath.Base(args[0]), "wpa_supplicant") {
			continue
		}
		on, own := false, false
		for i, a := range args {
			on = on || a == "-i"+iface || a == "-i" && i+1 < len(args) && args[i+1] == iface
			own = own || a == conf || a == "-c"+conf
		}
		if on || own {
			out[pid] = own
		}
	}
	return out
}

// killOwnSupplicants ends what a killed run left: wpa_supplicants with our
// config, never another one.
func killOwnSupplicants(proc, conf string) {
	for pid, own := range supplicants(proc, "", conf) {
		if !own {
			continue
		}
		logf("ending a wpa_supplicant a previous run left (pid %d)", pid)
		syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 30 && exists(filepath.Join(proc, strconv.Itoa(pid))); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
