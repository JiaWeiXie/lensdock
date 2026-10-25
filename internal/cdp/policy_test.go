package cdp

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPolicyInspect(t *testing.T) {
	p := DefaultPolicy()
	tests := []struct {
		name string
		msg  string
		want verdict
	}{
		{"allowed method", `{"id":1,"method":"Page.navigate","params":{"url":"https://example.com"}}`, verdictAllow},
		{"no method", `{"id":1}`, verdictAllow},
		{"non-string method", `{"id":1,"method":5}`, verdictAllow},
		{"method differs by case", `{"id":1,"method":"browser.close"}`, verdictAllow},
		{"method with trailing space", `{"id":1,"method":"Browser.close "}`, verdictAllow},
		{"capitalized key is not method", `{"id":1,"Method":"Browser.close"}`, verdictAllow},
		{"nested method is ignored", `{"id":1,"method":"Runtime.evaluate","params":{"method":"Browser.close"}}`, verdictAllow},
		{"domain that merely starts with Extensions", `{"id":1,"method":"ExtensionsX.foo"}`, verdictAllow},
		{"Extensions without a method name", `{"id":1,"method":"Extensions"}`, verdictAllow},
		{"Extensions domain differs by case", `{"id":1,"method":"extensions.uninstall"}`, verdictAllow},

		{"Browser.close", `{"id":1,"method":"Browser.close"}`, verdictDeny},
		{"Browser.crash", `{"id":1,"method":"Browser.crash"}`, verdictDeny},
		{"Browser.crashGpuProcess", `{"id":1,"method":"Browser.crashGpuProcess"}`, verdictDeny},
		{"Target.exposeDevToolsProtocol", `{"id":1,"method":"Target.exposeDevToolsProtocol","params":{"targetId":"x"}}`, verdictDeny},
		{"Extensions.loadUnpacked", `{"id":1,"method":"Extensions.loadUnpacked","params":{"path":"/x"}}`, verdictDeny},
		{"Extensions.uninstall", `{"id":1,"method":"Extensions.uninstall","params":{"id":"abc"}}`, verdictDeny},
		{"Extensions.getStorageItems", `{"id":1,"method":"Extensions.getStorageItems","params":{"id":"abc","storageArea":"local"}}`, verdictDeny},
		{"Extensions domain method not known today", `{"id":1,"method":"Extensions.somethingNew"}`, verdictDeny},
		{"Extensions method in a session", `{"id":1,"sessionId":"S1","method":"Extensions.uninstall"}`, verdictDeny},
		{"escaped key", `{"id":1,"m\u0065thod":"Browser.close"}`, verdictDeny},
		{"escaped value", `{"id":1,"method":"Browser\u002eclose"}`, verdictDeny},
		{"extra whitespace", " \n{ \"id\" : 1 , \"method\" : \"Browser.close\" }\n", verdictDeny},
		{"denied with different-case twin key", `{"id":1,"method":"Browser.close","METHOD":"Page.enable"}`, verdictDeny},
		{"session message", `{"id":1,"sessionId":"S1","method":"Browser.close"}`, verdictDeny},

		{"duplicate method", `{"id":1,"method":"Page.enable","method":"Browser.close"}`, verdictMalformed},
		{"duplicate id", `{"id":1,"id":2,"method":"Page.enable"}`, verdictMalformed},
		{"duplicate sessionId", `{"sessionId":"a","sessionId":"b","method":"Page.enable"}`, verdictMalformed},
		{"trailing object", `{"id":1,"method":"Page.enable"}{"id":2,"method":"Browser.close"}`, verdictMalformed},
		{"trailing garbage", `{"id":1,"method":"Page.enable"} x`, verdictMalformed},
		{"array", `[{"id":1,"method":"Browser.close"}]`, verdictMalformed},
		{"scalar", `"Browser.close"`, verdictMalformed},
		{"null", `null`, verdictMalformed},
		{"empty", ``, verdictMalformed},
		{"truncated", `{"id":1,"method":"Browser.close"`, verdictMalformed},
		{"trailing comma", `{"id":1,"method":"Page.enable",}`, verdictMalformed},
		{"comment", `{"id":1,/*x*/"method":"Page.enable"}`, verdictMalformed},
		{"single quotes", `{'id':1,'method':'Page.enable'}`, verdictMalformed},
		{"invalid utf-8", "{\"id\":1,\"method\":\"Page.\xff\"}", verdictMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := p.inspect([]byte(tt.msg)); got != tt.want {
				t.Fatalf("verdict = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSyntheticReplies(t *testing.T) {
	p := DefaultPolicy()
	tests := []struct {
		name, msg string
		wantID    any // nil: no id member
		wantSess  any
	}{
		{"numeric id", `{"id":7,"method":"Browser.close"}`, json.Number("7"), nil},
		{"string id", `{"id":"abc","method":"Browser.close"}`, "abc", nil},
		{"session id echoed", `{"id":9,"sessionId":"S-1","method":"Browser.close"}`, json.Number("9"), "S-1"},
		{"missing id", `{"method":"Browser.close"}`, nil, nil},
		{"object id is not echoed", `{"id":{"a":1},"method":"Browser.close"}`, nil, nil},
		{"non-string session is not echoed", `{"id":1,"sessionId":5,"method":"Browser.close"}`, json.Number("1"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, req := p.inspect([]byte(tt.msg))
			if v != verdictDeny {
				t.Fatalf("verdict = %d", v)
			}
			var got map[string]any
			dec := json.NewDecoder(bytes.NewReader(req.reply(-32000, deniedMessage)))
			dec.UseNumber()
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("reply is not JSON: %v", err)
			}
			if id, ok := got["id"]; (tt.wantID == nil) == ok || (ok && id != tt.wantID) {
				t.Errorf("id = %v (present %v), want %v", id, ok, tt.wantID)
			}
			if s, ok := got["sessionId"]; (tt.wantSess == nil) == ok || (ok && s != tt.wantSess) {
				t.Errorf("sessionId = %v (present %v), want %v", s, ok, tt.wantSess)
			}
			e, _ := got["error"].(map[string]any)
			if e == nil || e["code"] != json.Number("-32000") || e["message"] != "Method denied by LensDock policy" {
				t.Errorf("error = %v", got["error"])
			}
		})
	}
}
