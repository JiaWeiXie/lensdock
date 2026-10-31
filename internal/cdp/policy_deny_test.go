package cdp

import (
	"strings"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/lease"
)

func TestPolicyWithDenyAddsToTheDefaultAndNeverRemovesFromIt(t *testing.T) {
	none, err := PolicyWithDeny(nil)
	if err != nil {
		t.Fatal(err)
	}
	def := DefaultPolicy()
	for _, m := range []string{"Browser.crash", "Browser.crashGpuProcess", "Target.exposeDevToolsProtocol", "Target.sendMessageToTarget"} {
		if !none.Denied(m) || !def.Denied(m) {
			t.Errorf("built-in denial lost: %s", m)
		}
	}
	for _, m := range []string{"Network.getAllCookies", "Storage.getCookies", "Browser.close", "Extensions.loadUnpacked", "Page.navigate"} {
		if none.Denied(m) {
			t.Errorf("an empty list changed the default for %s", m)
		}
	}

	cookies, err := PolicyWithDeny([]string{"cookies"})
	if err != nil {
		t.Fatal(err)
	}
	for m, want := range map[string]bool{
		"Network.getAllCookies": true, "Network.getCookies": true, "Network.setCookie": true, "Network.setCookies": true,
		"Network.deleteCookies": true, "Network.clearBrowserCookies": true, "Storage.getCookies": true,
		"Storage.setCookies": true, "Storage.clearCookies": true, "Page.getCookies": true, "Page.deleteCookie": true,
		"Browser.crash": true,
		// not part of the preset
		"Network.enable": false, "Runtime.evaluate": false, "Storage.getUsageAndQuota": false, "Browser.close": false, "Page.navigate": false,
	} {
		if cookies.Denied(m) != want {
			t.Errorf("cookies preset: Denied(%q) = %v, want %v", m, !want, want)
		}
	}

	storage, _ := PolicyWithDeny([]string{"storage"})
	for m, want := range map[string]bool{
		"Storage.getUsageAndQuota": true, "IndexedDB.requestData": true, "CacheStorage.requestEntries": true,
		"DOMStorage.getDOMStorageItems": true, "Network.getAllCookies": false, "Page.navigate": false,
	} {
		if storage.Denied(m) != want {
			t.Errorf("storage preset: Denied(%q) = %v, want %v", m, !want, want)
		}
	}

	control, _ := PolicyWithDeny([]string{"browser-control"})
	for m, want := range map[string]bool{
		"Browser.close": true, "Browser.setDownloadBehavior": true, "Extensions.loadUnpacked": true, "Extensions.uninstall": true,
		"Browser.getVersion": false, "Extension.x": false,
	} {
		if control.Denied(m) != want {
			t.Errorf("browser-control preset: Denied(%q) = %v, want %v", m, !want, want)
		}
	}

	custom, err := PolicyWithDeny([]string{" cookies ", "", "Page.printToPDF", "Emulation.*"})
	if err != nil {
		t.Fatal(err)
	}
	if !custom.Denied("Page.printToPDF") || !custom.Denied("Emulation.setDeviceMetricsOverride") || !custom.Denied("Network.getAllCookies") {
		t.Fatal("custom entries were not applied")
	}
	if custom.Denied("Page.navigate") || custom.Denied("EmulationX.y") || custom.Denied("Emulation") {
		t.Fatal("custom entries matched too much")
	}
}

func TestPolicyWithDenyRejectsUnknownEntries(t *testing.T) {
	for _, bad := range []string{"cookie", "Cookies", "Network", "Network.", ".getCookies", "Network.get Cookies", "Network.*.x", "*", "*.get", "Net-work.get", "storage,cookies"} {
		if _, err := PolicyWithDeny([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		} else if !strings.Contains(err.Error(), "cookies") {
			t.Errorf("%q: the error does not name the presets: %v", bad, err)
		}
	}
}

func TestInspectDeniesConfiguredMethodsOnlyByExactDomainAndName(t *testing.T) {
	p, err := PolicyWithDeny([]string{"cookies", "Storage.*"})
	if err != nil {
		t.Fatal(err)
	}
	for msg, want := range map[string]verdict{
		`{"id":1,"method":"Network.getAllCookies"}`:                                   verdictDeny,
		`{"id":1,"sessionId":"S","method":"Storage.getUsageAndQuota"}`:                verdictDeny,
		`{"id":1,"method":"Network.getAllCookies","params":{"method":"Page.enable"}}`: verdictDeny,
		`{"id":1,"method":"Page.enable","params":{"method":"Network.getAllCookies"}}`: verdictAllow,
		`{"id":1,"method":"network.getAllCookies"}`:                                   verdictAllow, // Chromium matches case-sensitively
		`{"id":1,"method":"Network.getAllCookies","method":"Page.enable"}`:            verdictMalformed,
		"{\"id\":1,\"m\\u0065thod\":\"Network.getAllC\\u006fokies\"}":                 verdictDeny, // escaped key and value
	} {
		if got, _ := p.inspect([]byte(msg)); got != want {
			t.Errorf("%s: verdict %d, want %d", msg, got, want)
		}
	}
}

func TestProxyAppliesConfiguredDenyEntriesToALiveConnection(t *testing.T) {
	env := newRuntimeEnv(t, 0)
	policy, err := PolicyWithDeny([]string{"cookies"})
	if err != nil {
		t.Fatal(err)
	}
	env.proxy.policy = policy
	_, tok := env.acquire(t, lease.ModeAgent, "k1")
	c := mustDial(t, env.wsURL("/devtools/browser/"+browserID), tok.Reveal())

	writeText(t, c, `{"id":51,"method":"Network.getAllCookies"}`)
	_, data, err := readWithin(t, c, 2*time.Second)
	if err != nil || string(data) != `{"id":51,"error":{"code":-32000,"message":"Method denied by LensDock policy"}}` {
		t.Fatalf("reply = %s, %v", data, err)
	}
	expectEcho(t, c, `{"id":52,"method":"Page.navigate","params":{"url":"https://example.com"}}`)
	for _, m := range env.chromium.receivedMessages() {
		if strings.Contains(m, "getAllCookies") {
			t.Errorf("denied message reached Chromium: %s", m)
		}
	}
}
