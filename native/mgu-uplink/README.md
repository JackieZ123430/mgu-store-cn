# mgu-uplink

Internet for chosen apps through a USB network adapter, an LTE stick, a
USB-tethered phone or Wi-Fi, and for nothing else on the head unit.

The service brings up the device's interface as `vethuplink` (a name on
connman's interface blacklist, so connman leaves it alone; Wi-Fi keeps
`wlan1`, below), gets an address from the device by DHCP (the unit's
busybox `udhcpc`), and routes only the configured uids through it:

| What | Where |
|---|---|
| Address | on `vethuplink`, without a prefix route in the main table |
| Routes | table 110: the lease's subnet and a default via its router; `unreachable default` while offline |
| Rules | pref 1100 `uidrange U lookup main suppress_prefixlength 0` (car networks), 1101 `uidrange U lookup 110`, 1102 `from <lease address> lookup 110` |
| Firewall | `table inet mgu_uplink`: only the allowed uids leave through `vethuplink` (and `wlan1` while Wi-Fi is on), nothing connects in, nothing is forwarded; plus one `oifname "<interface>" accept comment "mgu-uplink"` per interface at the top of the unit's `ip filter output` chain |
| Lease for apps | `/var/opt/public/oap/oap_<uid>/data/uplink.json` per allowed app, `/run/mgu-uplink/lease.json` for everyone: `{interface, ip, prefix, router, dns[], leaseSeconds, updated}`, present only while online |
| Device state | `/run/mgu-uplink/status.json`, below |

The unit's default route, main table and resolver are not touched. The unit's
resolver does not reach the internet: allowed processes resolve names through
the lease's `dns` servers. While the uplink is down they get `EHOSTUNREACH`.

A stop (SIGTERM) removes everything above, gives a kernel adapter its own
name back and leaves the unit as before the start. A start first removes whatever
a killed run left, so stop, start and restart can be repeated freely.
`mgu-uplink clean` does the same removal by hand while the service is stopped;
`mgu-uplink status` shows what is in place.

## Uplinks

The service takes the first usable device, in this order, and checks again
every 2 s:

| Kind | Devices | How |
|---|---|---|
| `usb` | a USB Ethernet adapter the kernel drives (`asix`, `ax88179_178a`, `smsc95xx` on the unit), for example an AX88179 cabled to an LTE router | the kernel's interface, renamed to `vethuplink` and given its name back on stop |
| `usbecm` | a CDC-ECM stick: a Huawei HiLink stick (E3372h, `12d1:14db`/`14dc`) or any other ECM device | usbfs; a stick still presenting its install CD-ROM (`12d1:1f01`) is switched into network mode with the usb_modeswitch Huawei message |
| `wifi` | a saved Wi-Fi network in range (a phone's hotspot, an access point), while Wi-Fi is on | the unit's radio, `wlan1`, joined by our wpa_supplicant (wifi.go, below) |
| `rndis` | an Android phone in USB tethering: most phones (AOSP's default); control interface `e0/01/03`, `02/02/ff` or `ef/04/01` | usbfs, RNDIS (rndis.go) |
| `ncm` | an Android phone in USB tethering presenting CDC-NCM: Pixel 6 and later | usbfs, NCM transfer blocks (ncm.go) |
| `iphone` | an iPhone with Personal Hotspot on | usbfs: taken from the media daemon with the sysfs `authorized` toggle, configuration 4, paired over its usbmux interface (pairing.go), the hotspot interface as Linux's `ipheth` drives it (iphone.go) |

`uplink = netdev:<ifname>` in the config uses that existing interface
instead, under its own name (the emulator's second NIC).

A device earlier in the order is preferred: while the uplink runs on a
phone, a stick plugged in (or Wi-Fi joining) takes over within 5 s, and the
phone comes back when the stick is unplugged. One kind can be preferred
over all others (`preferred`, set by the Store through the `prefer`
request); the rest follow in the order above, as fallbacks. An unplugged
device ends its link, the service waits for the next one, and devices can
be swapped while it runs. A USB device that a kernel driver or another
process holds is left alone, except an iPhone, which the uplink takes from
the media daemon: so an iPhone is taken only while nothing ranked higher,
Wi-Fi to its hotspot included, is there.

The usbfs kinds share one relay: the device's bulk endpoints feed one end
of a veth pair, `vethuplinkp`; the other end is `vethuplink` with the MAC
the device assigns to the host and its checksum and segmentation offloads
off, so every frame reaches the device complete. Nothing loads or changes
kernel code (the unit's kernel has none of `cdc_ether`, `cdc_ncm`,
`rndis_host`, `ipheth`). The RNDIS driver follows the sequence of a
user-space RNDIS host proven with Android phones on the car: the RNDIS
control classes beyond `e0/01/03`, the data interface from the CDC Union, a
pad byte after a message that fills whole USB packets, the device's maximum
transfer size, its KEEPALIVE and status messages, and HALT on stop.

All of them run the same DHCP, routing, firewall and lease files.

### Phones

Android: plug the phone in and turn on Settings → Network → Hotspot &
tethering → USB tethering (most phones turn it off again when unplugged).

iPhone: turn on Settings → Personal Hotspot, plug the phone in and unlock
it. The first time, the phone asks "Trust This Computer?": tap Trust and
enter the passcode. Until then `status.json` says `trust-needed` with
`"Tap Trust on the iPhone and enter its passcode"` (or `"Unlock the
iPhone"`). After that the phone is paired with the unit and does not ask
again.

An iPhone serves its hotspot only to a host it trusts: the uplink pairs
with it the way a computer does (usbmuxd, libimobiledevice), over the
phone's usbmux interface (`ff/fe/02`). On every connection it opens a
lockdown session as the paired host. A phone that does not know the unit,
or has forgotten it (Settings → General → Transfer or Reset iPhone → Reset
Location & Privacy), is paired again. The unit's host identity (keys and
certificates, HostID, SystemBUID) is made once and kept in
`/var/opt/mgu-uplink/pairing/host.json` (root only), which updates keep;
deleting it makes the unit a new computer for every phone. When the owner
answers "Don't Trust", the phone goes back to media and is left alone
until it is plugged in again. A phone that trusts the unit but sends
nothing for 20 s is taken again, waiting twice as long each time in a row
(up to 5 minutes); a link whose usbmux session ended waits as long before
the phone is taken again. Data from the phone, or plugging it in again,
starts the wait over at 20 s.

On the head unit a stock media daemon grabs a
connected iPhone and holds it on configuration 2 (iAP/iPod media); the uplink
takes the device by deauthorizing and reauthorizing it through sysfs
(`/sys/bus/usb/devices/<X>/authorized`), then selecting configuration 4 and
claiming the hotspot interface before the media daemon puts the device back,
retried until it wins. While tethering the iPhone is a network device, so USB
media playback from that phone stops; it resumes when tethering ends (the
uplink reauthorizes the device on stop so the media daemon reclaims it). Only
that one Apple device is touched; the media daemon keeps serving every other
device. iOS's USB Restricted Mode cuts USB data after an hour locked unless
Settings → Face ID & Passcode → Accessories is on.

The iPhone takeover (authorized toggle, configuration 4) and the hotspot
interface's bring-up (alternate setting 1, GET_MACADDR, ENABLE_NCM,
CARRIER_CHECK, CLEAR_HALT on both bulk endpoints) follow IterioDev's
`ipheth-tether`. The two never drive the phone
at once: the unit has `Conflicts=ipheth-tether.service`, and the Store does
not install the tether (deploy/README.md, "IterioDev's App Manager").

### Wi-Fi

The unit joins a saved Wi-Fi network (a phone's hotspot, an access point)
with its own radio, and the uplink uses it like a plugged-in device: the
Wi-Fi provider (wifi.go) only associates, and DHCP, routing, rules,
firewall and lease are the ones above. Wi-Fi is off until switched on
(`wifi = on`, or the Store through `wifi-enable`); while off, the provider
leaves the radio and the interface alone.

On iDrive 8 (checked on a car) the radio is `wlan1`, the second adapter of
the NXP 88W9098: the first adapter's `wlan0` never sends an association
request. Both radios come rfkill soft-blocked; switched on, the provider
unblocks `wlan1`'s own phy only, turns IPv6 off on `wlan1` and brings it up,
and puts all three back when Wi-Fi is switched off or the service stops.
`wlan1` keeps its name: connman on the unit manages only the CarPlay
interfaces (`connmand -i wfd0,p2p-wfd0-0,p2p-wfd0-1`). An iPhone's hotspot
is seen with Settings → Personal Hotspot → Maximise Compatibility on
(2.4 GHz).

The association is our own wpa_supplicant (`wpa_supplicant`, beside
`mgu-uplink`): upstream 2.10 with [`wpa/wpa-mgu-sme-connect.patch`](wpa/wpa-mgu-sme-connect.patch).
The unit's driver (moal, `host_mlme=1`) reports the association that
wpa_supplicant's SME asked for as a connect event, which upstream ignores in
SME mode; the patch takes it as the association result. It runs on `wlan1`
only, never touches another wpa_supplicant (the stock one serves CarPlay on
`wfd0`), and is not started while another one runs on `wlan1` (link
`busy`). Saved networks are in `/var/opt/mgu-uplink/wifi/wpa.conf`, in a
folder only root can read; a network is saved once it was joined. WPA2 and
WPA3 personal (passwords of 8 to 63 characters) and open networks; no
enterprise (EAP).

### status.json

`/run/mgu-uplink/status.json` (0644, present while the service runs), for
the Store and the phone page:

```json
{ "device": { "kind": "iphone", "name": "Apple Inc. iPhone" },
  "state": "trust-needed", "error": "Tap Trust on the iPhone and enter its passcode" }
```

with `uplink = auto`, also `wifi`, and `preferred` while a kind is preferred:

```json
{ "device": { "kind": "wifi", "name": "iPhone - Mario" }, "state": "online", "error": null,
  "preferred": "wifi",
  "wifi": { "enabled": true, "link": "online", "ssid": "iPhone - Mario", "ip": "172.20.10.2", "signal": -45,
    "networks": [ { "ssid": "iPhone - Mario", "signal": -40, "secure": true, "saved": true, "current": true } ],
    "saved": [ "iPhone - Mario" ], "scanning": false, "error": null } }
```

| `wifi.link` | |
|---|---|
| `off` | Wi-Fi is switched off |
| `nohw` | the unit has no `wlan1` |
| `busy` | another wpa_supplicant runs on `wlan1`; left alone (`error`) |
| `failed` | the radio or the supplicant did not start (`error`); tried again |
| `idle`, `scanning`, `connecting` | not joined; `ssid` is the network being joined |
| `connected` | joined `ssid`; the uplink uses another device, or waits for DHCP |
| `online` | the uplink has a lease through it (`ip`) |

`networks` is the latest scan (the strongest per name, at most 20,
strongest first), `saved` the saved names, `scanning` true for 8 s after a
scan request, `signal` dBm. `error` keeps a failed join ("could not join
… (wrong password, or out of range)", after 40 s) until the next request.

| `state` | |
|---|---|
| `searching` | no usable device (`device` null) |
| `connecting` | the device is being set up, or waits for its DHCP lease; `error` may say why (`"Personal Hotspot is off"`) |
| `trust-needed` | an iPhone waits for its owner to tap Trust and enter the passcode, or to unlock it; `error` says what to do |
| `busy` | another driver or process holds the device, or the owner declined Trust on the iPhone (until it is plugged in again); it is left alone |
| `online` | there is a lease |
| `error` | setting the device up failed (`error`); it is tried again after 15 s |

`kind` is one of `usb`, `usbecm`, `wifi`, `rndis`, `ncm`, `iphone`,
`netdev`; `name` is the device's manufacturer and product strings, for
`wifi` the network's name.

### Requests

`/run/mgu-uplink/ctl.sock` (root only, 0600): one JSON object and a newline
in, one JSON line back, one request per connection. The Store talks to the
updater only, which relays these; `status.json` shows the results.

| Request | |
|---|---|
| `{"op": "prefer", "kind": "wifi"}` | the kind tried first (`usb`, `usbecm`, `wifi`, `rndis`, `ncm`, `iphone`; `""` for none); takes effect within 5 s |
| `{"op": "wifi-enable", "on": true}` | Wi-Fi on or off |
| `{"op": "wifi-scan"}` | scan now |
| `{"op": "wifi-connect", "ssid": "…", "psk": "…"}` | join a network, with its password; saved once joined. Without `psk`: a saved network, or an open one |
| `{"op": "wifi-forget", "ssid": "…"}` | forget a saved network |

The reply is `{"ok": true}` or `{"ok": false, "error": "…"}`. `prefer` and
`wifi-enable` are kept in `settings_conf`. A password goes to wpa_supplicant
only: no log line, reply or status names it.

## Config

The shipped [`mgu-uplink.conf`](mgu-uplink.conf) holds the defaults and is
replaced by every update. The unit's own settings go in
`/var/opt/mgu-uplink/mgu-uplink.local.conf` (same keys; each one given there
replaces the shipped value, except `uids`, which adds to it), which updates keep.
What the Store sets through requests goes in
`/var/opt/mgu-uplink/mgu-uplink.settings.conf`, read after the local conf
and kept by updates; versions before 0.7.0 do not read it, so a rollback
still starts.

| Key | Value |
|---|---|
| `apps` | OAP app names, resolved through `oap_12201/security/uids.txt` every 5 s |
| `uids` | numeric uids of non-app processes (the shipped list is empty); a local `uids` adds to the shipped one; 0 is refused |
| `uplink` | `auto`, or `netdev:<ifname>` for an existing interface (no Wi-Fi, no `preferred`) |
| `preferred` | the kind tried first; empty (shipped) for the order above |
| `wifi` | `on` or `off` (shipped) |
| `table`, `rule_pref` | routing table (110) and first rule priority (1100) |

Components that need the internet add themselves without touching the
conf: the deploy tool writes `/var/opt/mgu-uplink/uids.d/<component>`
(numeric uids) or `apps.d/<component>` (app names; an empty file stands
for the app named like the file), one per line, on install and removes it
on uninstall. The updater's downloads get out this way too, as
`uids.d/mgu-updater`, which names the uid the updater picked for them. They are read with the 5 s rescan, so an install or removal
takes effect within seconds; entries that are not a uid or an app name are
logged and skipped.

A device kind (`uplink = usbecm`, ...) or a `usb_device` left in a unit's
local conf is ignored, and the start logs it: the uplink takes whatever is
plugged in.

`uplink`, `table` and the other keys take effect at the next service
restart; `preferred` and `wifi` set by a request take effect at once.

## Build, install, update

```sh
make -C native/mgu-uplink target    # build/target/mgu-uplink (static linux/amd64) and build/target/wpa_supplicant
make -C native/mgu-uplink test      # Linux; on a Mac: test-docker
deploy/hu install mgu-uplink        # build and install over SSH (deploy/README.md)
```

`build/target/wpa_supplicant` is built with Docker (linux/amd64), in
Ubuntu 18.04 ([`wpa/Dockerfile`](wpa/Dockerfile)): the unit's glibc 2.27 and
loader `/lib/ld-linux-x86-64.so.2`, linked against the `libnl-3`,
`libnl-genl-3` and `libcrypto` 1.1 the unit has. The image holds upstream's
`wpa_supplicant-2.10.tar.gz`, checked by sha256; [`wpa/build.sh`](wpa/build.sh)
applies the patch and builds with the options in [`wpa/config`](wpa/config)
(nl80211, control socket, WPA2/WPA3 personal; no EAP, WPS, P2P or D-Bus),
and [`wpa/check.sh`](wpa/check.sh) checks the loader, the libraries and that
every symbol version is one the unit's libraries define. The same tree
gives the same bytes, so `release.js check` holds. It is rebuilt when
`wpa/` changes.

The unit tests cover the framing (RNDIS, NCM blocks with several datagrams
and chained NDPs, ipheth legacy and NCM receive), the descriptor matching,
the status file, iPhone pairing against a fake phone (usbmux, lockdown,
Pair with the Trust wait and its notification, the TLS session, a phone
that forgot the unit), and Wi-Fi: wpa_supplicant's control replies, and
the provider against a fake wpa_supplicant (the test binary run as one,
on its control socket) on a fake radio: off by default, radio unblocked
and blocked again, scan, a wrong password not saved, a join saved in a
root-only folder, the link ending when the supplicant joins another network
or the network is gone, another wpa_supplicant left alone, no password in
the log. Also the order with a preferred kind, the guarded interfaces, the
requests and `settings_conf`. [`test/vm-gadget.sh`](test/vm-gadget.sh) runs the uplinks
against USB gadgets in a Linux VM: ECM, NCM and RNDIS (f_rndis, and as
`e0/01/03` through a descriptor-rewriting usbip proxy) gadgets with a DHCP
server and NAT behind them, and a FunctionFS fake iPhone (gadget_test.go:
Apple ids, four configurations, the hotspot interface in NCM or legacy
framing, and a stand-in media daemon holding the device on configuration 2)
on dummy_hcd.

mgu-uplink is a tool component ([`component.json`](component.json)): the
deploy tool installs it, over SSH from the owner's computer
(`deploy/mgu-install.js first-install`, unless `--no-uplink`), or from the updater
(the Store's "Internet uplink" row: from the source that installed it, a
USB stick `usb-repo` wrote from that source included; one the installer put
there from the source listing it, of several the one listing it as
"Internet uplink", a stick included). The version is
`component.json`'s; `mgu-uplink --version` prints it.

| Path | What |
|---|---|
| `/var/opt/mgu-uplink/versions/<version>/` | `mgu-uplink`, `wpa_supplicant`, `mgu-uplink.conf`, `component.json` |
| `/var/opt/mgu-uplink/current` | the version in use |
| `/var/opt/mgu-uplink/mgu-uplink.local.conf` | the unit's settings |
| `/var/opt/mgu-uplink/mgu-uplink.settings.conf` | what the Store set (`preferred`, `wifi`) |
| `/var/opt/mgu-uplink/wifi/wpa.conf` | the saved Wi-Fi networks (folder 0700) |
| `/var/opt/mgu-uplink/uids.d/`, `apps.d/` | per-component drop-ins (the deploy tool's) |
| `/etc/systemd/system/mgu-uplink.service` | enabled for `multi-user.target` |
| `/etc/recmgr-restartlist.conf` | `mgu-uplink` in `[Whitelist]` and `[WhitelistExitSuccess]` |

Without the recmgr entries the head unit restarts whenever the service stops.
recmgr reads its list at boot, so a first install starts the service at the
next boot, and an update restarts it only once recmgr has loaded the entries;
until then the previous version keeps running. The restart removes what the
old version added and the new one adds it again, so allowed processes are
offline for the few seconds DHCP takes.

## Check it on the unit

The unit's `ip` (busybox) shows neither policy rules nor per-uid routes;
`mgu-uplink status` prints the device state, the preferred kind, Wi-Fi's
state, the lease, the rules, table 110, the firewall and the route to
1.1.1.1 for root and for each allowed uid.

```sh
systemctl status mgu-uplink; journalctl -u mgu-uplink -b
/var/opt/mgu-uplink/mgu-uplink status
cat /run/mgu-uplink/status.json
nft list table inet mgu_uplink; nft -a list chain ip filter output | grep mgu-uplink
ip route | grep -E 'vethuplink|wlan1'    # main table: nothing
```
