// SPDX-License-Identifier: LGPL-3.0-only
package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cprnative"
	"github.com/gin-gonic/gin"
)

// ForwardCPRModels bypasses local catalog synthesis, aliases and Lite overrides.
// Authentication and group model access restrictions remain enforced.
func (s *OpenAIGatewayService) ForwardCPRModels(c *gin.Context, group *Group) (*Account, error) {
	if !IsCPRNativeRequest(c) || group == nil { return nil, errors.New("CPR native model request not authorized") }
	snapshot, err := cprSnapshot(c)
	if err != nil { return nil, err }
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	var account *Account
	cfg := group.CodexModelsManifestConfig
	if cfg.Enabled {
		if len(cfg.AccountIDs) != 1 { return nil, errors.New("CPR原生目录只允许固定一个CPR账号；不合并能力目录") }
		members, listErr := s.accountRepo.ListByGroup(ctx, group.ID)
		if listErr != nil { return nil, errors.New("CPR模型目录账号查询失败") }
		for i := range members { if members[i].ID == cfg.AccountIDs[0] { account = &members[i]; break } }
		if account == nil { return nil, errors.New("固定CPR模型目录账号不属于当前分组") }
	} else {
		groupID := group.ID
		account, err = s.SelectAccountForModelWithExclusions(ctx, &groupID, "", "", map[int64]struct{}{})
		if err != nil { return nil, err }
	}
	if account == nil || !account.IsCPRUpstream() || !account.IsSchedulable() { return account, errors.New("没有可用的CPR模型目录账号") }
	if account.CPRNativeOwnerUserID() != c.GetInt64("cpr_native_owner_user_id") { return account, ErrCPRNativeAccessDenied }
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil { return account, err }
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil { return account, errors.New("CPR凭据不可用") }
	headers := snapshot.Header.Clone()
	if group.ModelAllowlistEnabled() {
		// Client validators can refer to a permission-filtered representation.
		headers.Del("If-None-Match"); headers.Del("If-Modified-Since")
	}
	req, err := cprnative.NewRequest(WithHTTPUpstreamRedirectsDisabled(ctx), base, "models", http.MethodGet, snapshot.RawQuery, headers, nil, token)
	if err != nil { return account, err }
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil { proxyURL = account.Proxy.URL() }
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil { return account, errors.New("获取CPR原生模型目录失败") }
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && !group.ModelAllowlistEnabled() {
		for k, v := range cprnative.ResponseHeaders(resp.Header) { c.Writer.Header()[k] = v }
		c.Status(http.StatusNotModified)
		return account, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil { return account, err }
	if len(body) > 8<<20 { return account, errors.New("CPR模型目录超过8MiB限制") }
	if resp.StatusCode != http.StatusOK {
		for k, v := range cprnative.ResponseHeaders(resp.Header) { c.Writer.Header()[k] = v }
		c.Status(resp.StatusCode)
		_, err = c.Writer.Write(body)
		return account, errors.Join(fmt.Errorf("CPR models returned HTTP %d", resp.StatusCode), err)
	}
	var allow func(string) bool
	if group.ModelAllowlistEnabled() { allow = func(model string) bool { return len(group.ModelAllowlist.FilterForListing([]string{model})) > 0 } }
	filtered, changed, err := cprnative.FilterCatalog(body, allow)
	if err != nil { return account, err }
	outgoing := cprnative.ResponseHeaders(resp.Header)
	etag := resp.Header.Get("ETag")
	if changed {
		etag = fmt.Sprintf("\"cpr-filter-%x\"", sha256.Sum256(filtered))
		outgoing.Del("Last-Modified"); outgoing.Del("Content-Encoding")
	}
	if etag != "" { outgoing.Set("ETag", etag) }
	for k, v := range outgoing { c.Writer.Header()[k] = v }
	if etag != "" && cprETagMatches(snapshot.Header.Get("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return account, nil
	}
	if c.Writer.Header().Get("Content-Type") == "" { c.Header("Content-Type", "application/json") }
	c.Status(http.StatusOK)
	_, err = c.Writer.Write(filtered)
	return account, err
}

func cprETagMatches(condition, etag string) bool {
	if condition == "" || etag == "" { return false }
	for _, candidate := range strings.Split(condition, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(etag, "W/") { return true }
	}
	return false
}
