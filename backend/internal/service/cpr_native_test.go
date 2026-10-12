package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func cprTestAccount(id, owner int64) Account {
	return Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{CPRUpstreamExtraKey: true, "cpr_owner_user_id": owner}}
}
func TestCPRNativeAccountFlagIsExplicitAndScoped(t *testing.T) {
	account := cprTestAccount(1, 42)
	if !account.IsCPRUpstream() { t.Fatal("explicit flag ignored") }
	account.Type = AccountTypeOAuth
	if account.IsCPRUpstream() { t.Fatal("OAuth must not enter API-key CPR path") }
	account.Type = AccountTypeAPIKey
	account.Platform = "anthropic"
	if account.IsCPRUpstream() { t.Fatal("wrong platform") }
	account.Platform = PlatformOpenAI
	account.Extra[CPRUpstreamExtraKey] = "true"
	if account.IsCPRUpstream() { t.Fatal("string must not enable native mode") }
}
func TestCPRNativeGroupAndTenantIsolation(t *testing.T) {
	a, b := cprTestAccount(1, 42), cprTestAccount(2, 42)
	enabled, owner, err := cprGroupMembersMode([]Account{a, b})
	if !enabled || owner != 42 || err != nil { t.Fatal(enabled, owner, err) }
	b.Extra["cpr_owner_user_id"] = 43
	if _, _, err := cprGroupMembersMode([]Account{a, b}); err == nil { t.Fatal("mixed users allowed") }
	b = cprTestAccount(2, 42)
	b.Extra[CPRUpstreamExtraKey] = false
	if _, _, err := cprGroupMembersMode([]Account{a, b}); err == nil { t.Fatal("normal-account fallback allowed") }
	if enabled, _, err := cprGroupMembersMode([]Account{b}); enabled || err != nil { t.Fatal("ordinary group changed") }
}
func TestCPRNativeOwnerIDParsing(t *testing.T) {
	for _, value := range []any{int(42), int64(42), float64(42), json.Number("42")} {
		a := cprTestAccount(1, 42); a.Extra["cpr_owner_user_id"] = value
		if a.CPRNativeOwnerUserID() != 42 { t.Fatal(value) }
	}
	for _, value := range []any{nil, -1, float64(1.5), "42", true, float64(1e20)} {
		a := cprTestAccount(1, 42); a.Extra["cpr_owner_user_id"] = value
		if a.CPRNativeOwnerUserID() != 0 { t.Fatal(value) }
	}
}

type cprFixtureRepo struct { AccountRepository; accounts []Account }
func (r *cprFixtureRepo) ListByGroup(context.Context, int64) ([]Account, error) { return r.accounts, nil }

func TestCPRNativeIngressKeepsOriginalHeaderAndBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{accountRepo: &cprFixtureRepo{accounts: []Account{cprTestAccount(1, 42)}}}
	group := &Group{ID: 7, Platform: PlatformOpenAI}
	router := gin.New()
	router.Use(CPRIngressSnapshotMiddleware())
	router.Use(func(c *gin.Context) { c.Request.Header.Set("x-client-request-id", "host-generated"); c.Next() })
	const body = `{ "model":"m", "reasoning":{"effort":"none"}, "unknown":{"x":1} }`
	router.POST("/v1/responses", func(c *gin.Context) {
		if err := svc.PrepareCPRIngress(c, group, 42); err != nil { t.Error(err); c.Status(500); return }
		original, err := cprSnapshot(c)
		if err != nil { t.Fatal(err) }
		if original.Header.Get("x-client-request-id") != "client-original" { t.Error("original request ID lost") }
		b, _ := io.ReadAll(c.Request.Body)
		if string(b) != body || string(original.Capture.Bytes) != body { t.Error("original body changed") }
		if !IsCPRNativeRequest(c) { t.Error("native marker not set") }
		c.Status(204)
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("x-client-request-id", "client-original")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 204 { t.Fatal(w.Code) }
}
func TestCPRNativeRejectsOtherUserAndWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{accountRepo: &cprFixtureRepo{accounts: []Account{cprTestAccount(1, 42)}}}
	group := &Group{ID: 7, Platform: PlatformOpenAI}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
	if err := svc.PrepareCPRIngress(c, group, 43); !errors.Is(err, ErrCPRNativeAccessDenied) { t.Fatal("wrong tenant accepted", err) }
	if err := svc.PrepareCPRIngress(c, group, 42); err == nil { t.Fatal("WS silently bridged") }
}
func TestCPRNativeConditionalCatalogETags(t *testing.T) {
	if !cprETagMatches(`W/"a", "b"`, `"a"`) { t.Fatal("weak GET validator not recognized") }
	if !cprETagMatches(`*`, `"x"`) { t.Fatal("wildcard not recognized") }
	if cprETagMatches(`"x"`, `"y"`) { t.Fatal("mismatched validator") }
}
