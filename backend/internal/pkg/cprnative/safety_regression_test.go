package cprnative

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCPRPOSTDoesNotPermitTransportReplay(t *testing.T) {
	req, err := NewRequest(context.Background(), "https://cpr.example.test/v1", "responses", "POST", "", http.Header{"Idempotency-Key": {"request-key"}}, []byte(`{"model":"m"}`), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody != nil {
		t.Fatal("stateful POST is replayable by the transport")
	}
	if req.Header.Get("Idempotency-Key") != "request-key" {
		t.Fatal("business header altered")
	}
}

func TestCPRRejectInvalidCacheAccounting(t *testing.T) {
	for _, value := range []string{"-1", "101", "null", "0.5", `"1"`} {
		observer := NewObserver(false, 4096)
		_, _ = observer.Write([]byte(`{"id":"resp_x","usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":` + value + `}}}`))
		if observer.Finish() == nil || observer.HasValidatedUsage() {
			t.Errorf("unsafe cached counter accepted: %s", value)
		}
	}
	good := NewObserver(false, 4096)
	_, _ = good.Write([]byte(`{"id":"resp_x","usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":90}}}`))
	if err := good.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestCPRObserverToolTrackingBound(t *testing.T) {
	observer := NewObserver(true, 4096)
	for i := 0; i < 65536; i++ {
		observer.imageIDs[strings.Repeat("x", i%10)+string(rune(i))] = true
	}
	// Fill with unambiguous decimal IDs in case a string encoding collides.
	for len(observer.imageIDs) < 65536 {
		observer.imageIDs[string(rune(len(observer.imageIDs)+0x10000))] = true
	}
	observer.observe([]byte(`{"type":"response.output_item.done","item":{"id":"new_id","type":"web_search_call","status":"completed"}}`), "")
	if observer.observationErr == nil {
		t.Fatal("unbounded tool tracking accepted")
	}
}
