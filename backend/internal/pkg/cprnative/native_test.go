package cprnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const nativeBody = `{
  "model": "codex-test", "stream": true,
  "reasoning": {"effort": "none"}, "prompt_cache_key": "cache-key-123",
  "tools": [{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /你好.*/"}},
            {"type":"namespace","name":"functions","tools":[{"type":"tool_search","defer_loading":true}]}],
  "input": [{"type":"additional_tools","tools":[{"type":"custom","name":"future","format":{"type":"text"}}]},
            {"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}],
  "client_metadata": {"session_id":"body-session", "unknown_future_key":{"a":[1,2,3]}},
  "unknown_future_field": {"x":true}
}`
const terminal = `{"type":"response.completed","response":{"id":"resp_native","model":"codex-test","service_tier":"default","usage":{"input_tokens":2000,"output_tokens":73,"input_tokens_details":{"cached_tokens":1536}},"output":[]}}`

func TestCPRNativeRequestBytesAndHeadersAtMockUpstream(t *testing.T) {
	headers := http.Header{}
	for _, k := range []string{"session-id", "thread-id", "x-client-request-id", "x-codex-turn-state", "x-codex-turn-metadata", "x-codex-installation-id", "x-codex-responses-lite", "x-codex-future-field"} {
		headers.Set(k, "original-"+k)
	}
	headers.Set("Authorization", "Bearer downstream-secret")
	headers.Set("X-API-Key", "downstream-secret-2")
	headers.Set("User-Agent", "Codex-Desktop-test")
	headers.Set("Cookie", "downstream-session")
	headers.Set("X-Forwarded-For", "192.0.2.1")
	headers.Set("Via", "proxy")
	headers.Set("Content-Type", "application/json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != nativeBody {
			t.Error("body was changed")
		}
		if r.Method != "POST" || r.URL.Path != "/v1/responses" || r.URL.RawQuery != "beta=1&beta=2" {
			t.Errorf("wrong target %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer cpr-secret" {
			t.Error("auth was not replaced")
		}
		for _, k := range []string{"session-id", "thread-id", "x-client-request-id", "x-codex-turn-state", "x-codex-turn-metadata", "x-codex-installation-id", "x-codex-responses-lite", "x-codex-future-field"} {
			if r.Header.Get(k) != "original-"+k {
				t.Errorf("changed header %s", k)
			}
		}
		for _, k := range []string{"Cookie", "X-Api-Key", "X-Forwarded-For", "Via"} {
			if r.Header.Get(k) != "" {
				t.Errorf("leaked header %s", k)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(terminal))
	}))
	defer server.Close()
	req, err := NewRequest(context.Background(), server.URL+"/v1", "responses", "POST", "beta=1&beta=2", headers, []byte(nativeBody), "cpr-secret")
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if headers.Get("Authorization") != "Bearer downstream-secret" {
		t.Fatal("input headers mutated")
	}
}

func TestCPRMissingHeadersNotFabricated(t *testing.T) {
	out := RequestHeaders(http.Header{}, "test-key")
	for _, k := range []string{"session-id", "session_id", "thread-id", "x-client-request-id", "x-codex-turn-state"} {
		if _, exists := out[http.CanonicalHeaderKey(k)]; exists {
			t.Errorf("fabricated %s", k)
		}
	}
	if out.Get("User-Agent") != "" {
		t.Fatal("invented user agent")
	}
}
func TestCPRHeaderAliasPreservedSeparately(t *testing.T) {
	h := http.Header{}
	h.Set("session-id", "new")
	h.Set("session_id", "old")
	out := RequestHeaders(h, "key")
	if out.Get("session-id") != "new" || out.Get("session_id") != "old" {
		t.Fatal("aliases collapsed")
	}
}
func TestCPRHopByHopAndDynamicConnectionHeaders(t *testing.T) {
	h := http.Header{"Connection": {"keep-alive, X-Hop"}, "X-Hop": {"remove"}, "X-End-To-End": {"preserve"}, "Upgrade": {"websocket"}, "Proxy-Authorization": {"secret"}}
	for _, out := range []http.Header{RequestHeaders(h, "key"), ResponseHeaders(h)} {
		for _, k := range []string{"Connection", "X-Hop", "Upgrade", "Proxy-Authorization"} {
			if out.Get(k) != "" {
				t.Errorf("hop header leaked: %s", k)
			}
		}
		if out.Get("X-End-To-End") != "preserve" {
			t.Error("end-to-end header removed")
		}
	}
}
func TestCPRResponseSecurityBoundary(t *testing.T) {
	in := http.Header{}
	in.Add("Set-Cookie", "session=evil")
	in.Set("Access-Control-Allow-Origin", "*")
	in.Set("Content-Security-Policy", "unsafe")
	in.Set("x-codex-turn-state", "state-value")
	in.Set("X-Future-Protocol", "ok")
	out := ResponseHeaders(in)
	for _, k := range []string{"Set-Cookie", "Access-Control-Allow-Origin", "Content-Security-Policy"} {
		if out.Get(k) != "" {
			t.Error(k)
		}
	}
	if out.Get("x-codex-turn-state") != "state-value" || out.Get("X-Future-Protocol") != "ok" {
		t.Fatal("protocol fields lost")
	}
}
func TestCPREndpointValidation(t *testing.T) {
	for _, base := range []string{"https://u:p@example.com/v1", "https://example.com/v1?key=secret", "https://example.com/v1?", "https://example.com/v1#fragment", "file:///v1", "https://example.com/v1/responses", "https://example.com/v2", "https://example.com/v%31"} {
		if _, err := EndpointURL(base, "responses", ""); err == nil {
			t.Errorf("accepted bad base %q", base)
		}
	}
	for _, query := range []string{"api_key=x", "API_KEY=x", "access%5ftoken=x", "key=x", "token=x"} {
		if _, err := EndpointURL("https://example.com/v1", "responses", query); err == nil {
			t.Errorf("query credentials accepted %q", query)
		}
	}
	if _, err := NewRequest(context.Background(), "https://example.com/v1", "responses", "GET", "", nil, nil, "key"); err == nil {
		t.Fatal("WS GET accepted by HTTP path")
	}
	if _, err := NewRequest(context.Background(), "https://example.com/v1", "responses", "POST", "", nil, nil, "key\r\nHost: evil"); err == nil {
		t.Fatal("header injection accepted")
	}
}
func TestCPRValidJSONPreserved(t *testing.T) {
	if err := ValidateJSON([]byte(nativeBody)); err != nil {
		t.Fatal(err)
	}
}
func TestCPRRejectAmbiguousJSON(t *testing.T) {
	bad := []string{`{"model":"a","model":"b"}`, `{"x":{"a":1,"\u0061":2}}`, `{} {}`, `{"a":`, strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130), string([]byte{'"', 0xff, '"'})}
	for _, input := range bad {
		if err := ValidateJSON([]byte(input)); err == nil {
			t.Errorf("accepted invalid or ambiguous JSON %q", input)
		}
	}
}
func TestCPRInspectStrictModelStream(t *testing.T) {
	for _, input := range []string{`[]`, `null`, `{"model":1}`, `{"model":""}`, `{"model":"m","stream":null}`, `{"model":"m","stream":"true"}`} {
		if _, err := InspectRequest([]byte(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	info, err := InspectRequest([]byte(nativeBody))
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "codex-test" || !info.Stream || info.Effort != "none" {
		t.Fatalf("wrong info %+v", info)
	}
}
func TestCPRPolicyConflictDetection(t *testing.T) {
	body := []byte(`{"model":"m","reasoning":{"effort":"none"},"service_tier":"default"}`)
	equivalent := []byte(`{"service_tier": "default", "reasoning":{"effort":"none"}, "model":"m", "input":[]}`)
	if !PolicyFieldsEqual(body, equivalent) {
		t.Fatal("formatting should not be a conflict")
	}
	for _, other := range []string{`{"model":"other"}`, `{"model":"m","reasoning":{},"service_tier":"default"}`, `{"model":"m","reasoning":{"effort":"high"},"service_tier":"default"}`} {
		if PolicyFieldsEqual(body, []byte(other)) {
			t.Fatal("missed configured override")
		}
	}
	if PolicyFieldsEqual([]byte(`{"model":"m"}`), []byte(`{"model":"m","reasoning":null}`)) {
		t.Fatal("absent and null collapsed")
	}
}
func TestCPRCaptureBoundAndNoReaderDisruption(t *testing.T) {
	c := Capture{Limit: 8}
	input := []byte("0123456789abc")
	if n, err := c.Write(input); n != len(input) || err != nil {
		t.Fatal(n, err)
	}
	if !c.Overflow || string(c.Bytes) != "01234567" {
		t.Fatal("bad overflow capture")
	}
	if n, err := c.Write(input); n != len(input) || err != nil || len(c.Bytes) != 8 {
		t.Fatal("overflow disrupted host read")
	}
}

func TestCPRSSEChunkBoundariesAndExactRelay(t *testing.T) {
	payload := "\xef\xbb\xbf: heartbeat\r\nevent: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\"}}\r\n\r\n" +
		"event: response.output_text.delta\n" + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n" +
		"event: response.completed\r\ndata: " + terminal + "\r\n\r\ndata: [DONE]\n\n"
	for size := 1; size <= 31; size++ {
		observer := NewObserver(true, 4096)
		var out bytes.Buffer
		reader := &chunkReader{data: []byte(payload), chunk: size}
		disconnected, err := Relay(&out, reader, observer, nil, nil)
		if err != nil || disconnected {
			t.Fatal(err)
		}
		if out.String() != payload {
			t.Fatalf("changed bytes at chunk %d", size)
		}
		if err := observer.Finish(); err != nil {
			t.Fatalf("chunk %d: %v", size, err)
		}
		if observer.ResponseID != "resp_native" || observer.Model != "codex-test" {
			t.Fatalf("bad metadata %+v", observer)
		}
		var usage map[string]any
		_ = json.Unmarshal(observer.UsageJSON, &usage)
		if usage["input_tokens"] != float64(2000) || usage["output_tokens"] != float64(73) {
			t.Fatal("wrong usage")
		}
	}
}

type chunkReader struct {
	data  []byte
	chunk int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(r.data), r.chunk)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestCPRSSEMultilineAndLoneCR(t *testing.T) {
	payload := "event: response.completed\rdata: {\"type\":\"response.completed\",\rdata: \"response\":{\"id\":\"resp_m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\r\r"
	o := NewObserver(true, 4096)
	_, _ = o.Write([]byte(payload))
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
	if o.ResponseID != "resp_m" {
		t.Fatal(o.ResponseID)
	}
}
func TestCPRNonstreamJSONAccounting(t *testing.T) {
	payload := `{"id":"resp_json","model":"m","status":"completed","usage":{"input_tokens":12,"output_tokens":3},"output":[]}`
	o := NewObserver(false, 4096)
	_, _ = o.Write([]byte(payload))
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
	if o.ResponseID != "resp_json" || o.Terminal != "response.completed" {
		t.Fatal("wrong unary metadata")
	}
}
func TestCPRExplicitZeroUsageIsNotMissing(t *testing.T) {
	o := NewObserver(false, 4096)
	_, _ = o.Write([]byte(`{"id":"resp_zero","usage":{"input_tokens":0,"output_tokens":0}}`))
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
}
func TestCPRMissingAndMalformedUsageNotFaked(t *testing.T) {
	for _, usage := range []string{`null`, `{}`, `{"input_tokens":1}`, `{"input_tokens":null,"output_tokens":1}`, `{"input_tokens":-1,"output_tokens":1}`, `{"input_tokens":1.5,"output_tokens":1}`, `{"input_tokens":"4","output_tokens":1}`} {
		o := NewObserver(false, 4096)
		_, _ = o.Write([]byte(`{"usage":` + usage + `}`))
		if err := o.Finish(); err == nil {
			t.Fatalf("accepted bad usage %s", usage)
		}
	}
}
func TestCPRDuplicateUsageNotSummed(t *testing.T) {
	o := NewObserver(true, 4096)
	frame := "data: " + terminal + "\n\n"
	_, _ = o.Write([]byte(frame + frame))
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(o.UsageJSON), `"input_tokens":2000`) {
		t.Fatal("usage changed")
	}
}
func TestCPRTruncatedStreamNotSuccess(t *testing.T) {
	o := NewObserver(true, 4096)
	_, _ = o.Write([]byte("data: " + terminal))
	if err := o.Finish(); err == nil {
		t.Fatal("unterminated event accepted")
	}
	o = NewObserver(true, 4096)
	_, _ = o.Write([]byte(`data: {"type":"response.in_progress","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))
	if !errors.Is(o.Finish(), ErrTerminalMissing) {
		t.Fatal("missing terminal accepted")
	}
}
func TestCPROversizedDeltaDoesNotTruncateRelay(t *testing.T) {
	payload := "data: " + strings.Repeat("x", 5000) + "\n\n" + "data: " + terminal + "\n\n"
	o := NewObserver(true, 1024)
	var out bytes.Buffer
	_, err := Relay(&out, strings.NewReader(payload), o, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != payload {
		t.Fatal("large event truncated")
	}
	if o.SkippedEvents != 1 {
		t.Fatal("large event not bounded")
	}
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
}
func TestCPROversizedUsageFailsAccounting(t *testing.T) {
	o := NewObserver(false, 64)
	_, _ = o.Write([]byte(terminal))
	if o.Finish() == nil {
		t.Fatal("overflow became zero bill")
	}
}
func TestCPRResponseIDConflictDetected(t *testing.T) {
	o := NewObserver(true, 4096)
	_, _ = o.Write([]byte("data: " + terminal + "\n\n" + "data: " + strings.Replace(terminal, "resp_native", "resp_other", 1) + "\n\n"))
	if o.Finish() == nil {
		t.Fatal("multiple responses became one charge")
	}
}

type failWriter struct{ writes int }

func (w *failWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errors.New("client disconnected")
}
func TestCPRDisconnectStillObservesActualUsage(t *testing.T) {
	payload := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n" + "data: " + terminal + "\n\n"
	o := NewObserver(true, 4096)
	dst := &failWriter{}
	callbacks := 0
	disconnected, err := Relay(dst, &chunkReader{data: []byte(payload), chunk: 8}, o, nil, func() { callbacks++ })
	if !disconnected || err == nil || dst.writes != 1 || callbacks != 1 {
		t.Fatal("incorrect disconnect handling")
	}
	if err := o.Finish(); err != nil {
		t.Fatal("usage was not drained", err)
	}
}
func TestCPRNativeOutputCountersDeduplicate(t *testing.T) {
	item := `{"type":"image_generation_call","id":"img_1","result":"base64","status":"completed"}`
	event := `{"type":"response.output_item.done","item":` + item + `}`
	completion := `{"type":"response.completed","response":{"id":"resp_i","usage":{"input_tokens":2,"output_tokens":3},"output":[` + item + `,{"type":"web_search_call","id":"s","status":"completed"}]}}`
	o := NewObserver(true, 4096)
	_, _ = o.Write([]byte("data: " + event + "\n\ndata: " + event + "\n\ndata: " + completion + "\n\n"))
	if err := o.Finish(); err != nil {
		t.Fatal(err)
	}
	if o.ImageCount != 1 || o.SearchCount != 1 {
		t.Fatalf("wrong counters %d %d", o.ImageCount, o.SearchCount)
	}
}
func TestCPRNativeCatalogUnchanged(t *testing.T) {
	catalog := []byte(`{ "models": [{"slug":"gpt-6-astra","use_responses_lite":true,"future":{"grammar":"start: /./"}}], "unknown": 123 }`)
	for _, allow := range []func(string) bool{nil, func(string) bool { return true }} {
		out, changed, err := FilterCatalog(catalog, allow)
		if err != nil || changed || !bytes.Equal(out, catalog) {
			t.Fatal("native catalog modified", err)
		}
	}
}
func TestCPRNativeCatalogPermissionFilteringOnly(t *testing.T) {
	catalog := []byte(`{"models":[{"slug":"allowed","use_responses_lite":true,"future":{"a":[1,2]}},{"slug":"denied","use_responses_lite":true}],"unknown":"keep"}`)
	out, changed, err := FilterCatalog(catalog, func(id string) bool { return id == "allowed" })
	if err != nil || !changed {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(out, &root)
	var entries []map[string]any
	_ = json.Unmarshal(root["models"], &entries)
	if len(entries) != 1 || entries[0]["slug"] != "allowed" || entries[0]["use_responses_lite"] != true || string(root["unknown"]) != `"keep"` {
		t.Fatal("model capabilities changed")
	}
}
func TestCPROpenAIListCatalogPreserved(t *testing.T) {
	b := []byte(`{"object":"list","data":[{"id":"m","unknown":{"native":true}}]}`)
	out, changed, err := FilterCatalog(b, func(id string) bool { return id == "m" })
	if err != nil || changed || !bytes.Equal(out, b) {
		t.Fatal(err)
	}
}
func TestCPRConcurrentRequestsDoNotShareMetadata(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := http.Header{"Session-Id": {"s"}}
			out := RequestHeaders(in, "k")
			out.Set("Session-Id", "other")
			if !reflect.DeepEqual(in, http.Header{"Session-Id": {"s"}}) {
				t.Error("shared header storage")
			}
			o := NewObserver(false, 4096)
			_, _ = o.Write([]byte(`{"id":"r","usage":{"input_tokens":1,"output_tokens":2}}`))
			if err := o.Finish(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
func FuzzCPRValidateJSON(f *testing.F) {
	for _, s := range []string{nativeBody, `{}`, `{"a":1,"a":2}`, `[1,2]`, "null"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 128<<10 {
			return
		}
		_ = ValidateJSON(b)
	})
}

func TestCPRInterruptedStreamKeepsValidatedPartialUsage(t *testing.T) {
	o := NewObserver(true, 4096)
	_, _ = o.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_partial\",\"usage\":{\"input_tokens\":200,\"output_tokens\":7}}}\n\n"))
	if !errors.Is(o.Finish(), ErrTerminalMissing) {
		t.Fatal("must report incomplete stream")
	}
	if !o.HasValidatedUsage() {
		t.Fatal("observed partial usage must remain available for billing")
	}
}
