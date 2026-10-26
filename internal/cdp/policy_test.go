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
		{"method differs by case", `{"id":1,"method":"browser.crash"}`, verdictAllow},
		{"method with trailing space", `{"id":1,"method":"Browser.crash "}`, verdictAllow},
		{"capitalized key is not method", `{"id":1,"Method":"Browser.crash"}`, verdictAllow},
		{"nested method is ignored", `{"id":1,"method":"Runtime.evaluate","params":{"method":"Browser.crash"}}`, verdictAllow},
		{"Browser.close ends the browser for the lease holder", `{"id":1,"method":"Browser.close"}`, verdictAllow},
		{"Extensions.loadUnpacked", `{"id":1,"method":"Extensions.loadUnpacked","params":{"path":"/x"}}`, verdictAllow},
		{"Extensions.uninstall", `{"id":1,"method":"Extensions.uninstall","params":{"id":"abc"}}`, verdictAllow},
		{"Extensions.getStorageItems in a session", `{"id":1,"sessionId":"S1","method":"Extensions.getStorageItems","params":{"id":"abc","storageArea":"local"}}`, verdictAllow},
		{"flattened Target.attachToTarget", `{"id":1,"method":"Target.attachToTarget","params":{"targetId":"x","flatten":true}}`, verdictAllow},

		{"Browser.crash", `{"id":1,"method":"Browser.crash"}`, verdictDeny},
		{"Browser.crashGpuProcess", `{"id":1,"method":"Browser.crashGpuProcess"}`, verdictDeny},
		{"Target.exposeDevToolsProtocol", `{"id":1,"method":"Target.exposeDevToolsProtocol","params":{"targetId":"x"}}`, verdictDeny},
		{"Target.exposeDevToolsProtocol in a session", `{"id":1,"sessionId":"S1","method":"Target.exposeDevToolsProtocol","params":{"targetId":"x"}}`, verdictDeny},
		{"Target.sendMessageToTarget carries uninspected messages", `{"id":1,"method":"Target.sendMessageToTarget","params":{"sessionId":"S1","message":"{}"}}`, verdictDeny},
		{"escaped key", `{"id":1,"m\u0065thod":"Browser.crash"}`, verdictDeny},
		{"escaped value", `{"id":1,"method":"Browser\u002ecrash"}`, verdictDeny},
		{"extra whitespace", " \n{ \"id\" : 1 , \"method\" : \"Browser.crash\" }\n", verdictDeny},
		{"denied with different-case twin key", `{"id":1,"method":"Browser.crash","METHOD":"Page.enable"}`, verdictDeny},
		{"session message", `{"id":1,"sessionId":"S1","method":"Browser.crash"}`, verdictDeny},

		{"duplicate method", `{"id":1,"method":"Page.enable","method":"Browser.crash"}`, verdictMalformed},
		{"duplicate id", `{"id":1,"id":2,"method":"Page.enable"}`, verdictMalformed},
		{"duplicate sessionId", `{"sessionId":"a","sessionId":"b","method":"Page.enable"}`, verdictMalformed},
		{"trailing object", `{"id":1,"method":"Page.enable"}{"id":2,"method":"Browser.crash"}`, verdictMalformed},
		{"trailing garbage", `{"id":1,"method":"Page.enable"} x`, verdictMalformed},
		{"array", `[{"id":1,"method":"Browser.crash"}]`, verdictMalformed},
		{"scalar", `"Browser.crash"`, verdictMalformed},
		{"null", `null`, verdictMalformed},
		{"empty", ``, verdictMalformed},
		{"truncated", `{"id":1,"method":"Browser.crash"`, verdictMalformed},
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
		{"numeric id", `{"id":7,"method":"Browser.crash"}`, json.Number("7"), nil},
		{"string id", `{"id":"abc","method":"Browser.crash"}`, "abc", nil},
		{"session id echoed", `{"id":9,"sessionId":"S-1","method":"Browser.crash"}`, json.Number("9"), "S-1"},
		{"missing id", `{"method":"Browser.crash"}`, nil, nil},
		{"object id is not echoed", `{"id":{"a":1},"method":"Browser.crash"}`, nil, nil},
		{"non-string session is not echoed", `{"id":1,"sessionId":5,"method":"Browser.crash"}`, json.Number("1"), nil},
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
