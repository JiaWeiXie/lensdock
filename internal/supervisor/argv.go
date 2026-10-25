package supervisor

import "strconv"

// Fixed runtime-container identities and paths.
const (
	supervisorUID = 10000
	supervisorGID = 10000
	browserUID    = 10001
	browserGID    = 10001

	display        = ":99"
	displayNumber  = 99
	screenGeometry = "1920x1080x24"

	// ScreenWidth and ScreenHeight are the display size Xvfb starts with. A
	// RESIZE request may shrink it to another advertised mode; it can never
	// grow beyond these.
	ScreenWidth  = 1920
	ScreenHeight = 1080

	runDir         = "/run/lensdock"
	browserRunDir  = "/run/lensdock/browser"
	xauthorityPath = "/run/lensdock/browser/Xauthority"
	dbusSocketPath = "/run/lensdock/browser/dbus.sock"
	dbusAddress    = "unix:path=" + dbusSocketPath
	xdgRuntimeDir  = "/run/user/10001"
	x11SocketDir   = "/tmp/.X11-unix"

	profileDir   = "/data/profile"
	downloadsDir = "/data/downloads"

	chromiumBinary = "/usr/lib/chromium/chromium"
	cacheDir       = "/home/browser/.cache/chromium"

	cdpHost = "127.0.0.1"
	cdpPort = 9223

	manifestPath = "/usr/share/lensdock/dpkg-manifest.tsv"
	openboxRC    = "/etc/lensdock/openbox/rc.xml"
)

var cdpAddr = cdpHost + ":" + strconv.Itoa(cdpPort)

// ChromiumArgv returns the complete, fixed Chromium command line (argv[0] is
// the binary). There is deliberately no way to add flags: the sandbox stays
// on and cannot be weakened by configuration.
func ChromiumArgv() []string {
	return []string{
		chromiumBinary,
		"--user-data-dir=" + profileDir,
		"--remote-debugging-address=" + cdpHost,
		"--remote-debugging-port=" + strconv.Itoa(cdpPort),
		"--no-first-run",
		"--no-default-browser-check",
		"--lang=en-US",
		"--ozone-platform=x11",
		"--window-position=0,0",
		"--window-size=1920,1080",
		"--start-maximized",
		"--disk-cache-dir=" + cacheDir,
		"about:blank",
	}
}

// childEnv is the explicit environment allowlist for every child process.
func childEnv() []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"DISPLAY=" + display,
		"XAUTHORITY=" + xauthorityPath,
		"HOME=/home/browser",
		"XDG_RUNTIME_DIR=" + xdgRuntimeDir,
		"DBUS_SESSION_BUS_ADDRESS=" + dbusAddress,
		"LANG=en_US.UTF-8",
		// Keep generic families Latin so CJK glyphs fall back by element lang.
		// A CJK generic default can swallow Japanese and Korean glyph selection.
		"FC_LANG=en",
		"TZ=Asia/Taipei",
	}
}
