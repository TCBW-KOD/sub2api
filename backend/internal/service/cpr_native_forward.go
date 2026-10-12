// SPDX-License-Identifier: LGPL-3.0-only
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cprnative"
	"github.com/gin-gonic/gin"
)

// Called before every compatibility transform in Forward. The original handler
// retains authentication, permissions, balance reservation, account concurrency,
// pricing and RecordUsage. No alternate billing implementation is introduced.
func (s *OpenAIGatewayService) forwardCPRNative(ctx context.Context, c *gin.Context, account *Account, attemptBody []byte) (*OpenAIForwardResult, error) {
	if !IsCPRNativeRequest(c) {
		return nil, cprReject(c, 409, "cpr_native_group_required", "CPR账号必须位于独立OpenAI原生分组；拒绝普通兼容路径")
	}
	snapshot, err := cprSnapshot(c)
	if err != nil { return nil, cprReject(c, 500, "cpr_snapshot_unavailable", err.Error()) }
	if !snapshot.Complete || snapshot.Capture.Overflow || snapshot.ReadError != nil {
		return nil, cprReject(c, 400, "cpr_original_body_unavailable", "完整原始正文不可用；不会转发截断或已改写正文")
	}
	if cprEndpoint(snapshot.Path) != "responses" || c.Request.Method != http.MethodPost {
		return nil, cprReject(c, 400, "cpr_native_endpoint_unsupported", "CPR native endpoint is not supported by this build")
	}
	body := snapshot.Capture.Bytes
	info, err := cprnative.InspectRequest(body)
	if err != nil { return nil, cprReject(c, 400, "cpr_invalid_json", err.Error()) }
	if !cprnative.PolicyFieldsEqual(body, attemptBody) || account.GetMappedModel(info.Model) != info.Model {
		return nil, cprReject(c, 409, "cpr_native_policy_conflict", "模型/推理/Fast配置要求改写请求；请取消对应改写策略")
	}
	// Existing policy can reject a request. A requested mutation is a conflict,
	// never an excuse to bypass the policy or to silently change business bytes.
	policyBody, err := s.applyOpenAIFastPolicyToBody(ctx, account, info.Model, bytes.Clone(body))
	if err != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(err, &blocked) { writeOpenAIFastPolicyBlockedResponse(c, blocked); return nil, err }
		return nil, cprReject(c, 400, "cpr_policy_rejected", err.Error())
	}
	if !bytes.Equal(policyBody, body) {
		return nil, cprReject(c, 409, "cpr_native_policy_conflict", "Fast策略将改写正文；CPR原生模式不执行改写")
	}
	restriction := s.detectCodexClientRestriction(c, account, body)
	if restriction.Enabled && !restriction.Matched {
		return nil, cprReject(c, 403, "cpr_client_restricted", "This account is restricted to the configured Codex client")
	}
	fresh, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil || fresh == nil || !fresh.IsCPRUpstream() || !fresh.IsSchedulable() {
		return nil, cprReject(c, 503, "cpr_account_unavailable", "CPR账号不可调度或原生标记已更改")
	}
	if fresh.CPRNativeOwnerUserID() != c.GetInt64("cpr_native_owner_user_id") {
		return nil, cprReject(c, 403, "cpr_native_owner_changed", "CPR账号允许用户已变更")
	}
	if fresh.GetMappedModel(info.Model) != info.Model {
		return nil, cprReject(c, 409, "cpr_native_policy_conflict", "CPR账号模型映射与原生请求冲突")
	}
	base, err := s.validateUpstreamBaseURL(fresh.GetCredential("base_url"))
	if err != nil { return nil, cprReject(c, 400, "cpr_invalid_base_url", err.Error()) }
	token, _, err := s.GetAccessToken(ctx, fresh)
	if err != nil { return nil, cprReject(c, 502, "cpr_credentials_unavailable", "CPR凭据不可用") }
	if err := ctx.Err(); err != nil { return nil, err }

	start := time.Now()
	upstreamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cprMaxRequestDuration)
	defer cancel()
	disconnectSignal := make(chan struct{})
	var disconnectOnce sync.Once
	markDisconnected := func() { disconnectOnce.Do(func() { close(disconnectSignal) }) }
	go func(drainCtx context.Context) {
		select { case <-ctx.Done(): case <-disconnectSignal: case <-drainCtx.Done(): return }
		timer := time.NewTimer(cprDisconnectDrainBudget)
		defer timer.Stop()
		select { case <-timer.C: cancel(); case <-drainCtx.Done(): }
	}(upstreamCtx)
	upstreamCtx = WithHTTPUpstreamRedirectsDisabled(upstreamCtx)
	upstreamCtx = WithHTTPUpstreamProfile(upstreamCtx, HTTPUpstreamProfileLongStream)
	req, err := cprnative.NewRequest(upstreamCtx, base, "responses", http.MethodPost, snapshot.RawQuery, snapshot.Header, body, token)
	if err != nil { return nil, cprReject(c, 400, "cpr_invalid_request", err.Error()) }
	proxyURL := ""
	if fresh.ProxyID != nil && fresh.Proxy != nil { proxyURL = fresh.Proxy.URL() }
	SetActualOpenAIUpstreamEndpoint(c, "/v1/responses")
	SetOpsUpstreamModel(c, info.Model)
	resp, err := s.doOpenAIUpstream(req, proxyURL, fresh)
	if err != nil {
		// Never return UpstreamFailoverError after a stateful POST. It is unknown
		// whether CPR executed the request; recovery/replay belongs to CPR/client.
		return nil, cprReject(c, 502, "cpr_transport_failed", "连接CPR失败；未自动重放本次请求")
	}
	defer resp.Body.Close()
	flush := func() error { return http.NewResponseController(c.Writer).Flush() }
	writeHeaders := func() {
		for key, values := range cprnative.ResponseHeaders(resp.Header) { c.Writer.Header()[key] = values }
		c.Status(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeHeaders()
		_, copyErr := cprnative.Relay(c.Writer, resp.Body, nil, flush, markDisconnected)
		return nil, errors.Join(fmt.Errorf("CPR returned HTTP %d; response forwarded unchanged", resp.StatusCode), copyErr)
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	sse := media == "text/event-stream"
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if (!sse && media != "application/json") || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
		err := errors.New("CPR success response cannot be metered as decoded JSON/SSE")
		s.quarantineCPRAccountingGap(ctx, account, "", err)
		return nil, cprReject(c, 502, "cpr_unmeterable_response", err.Error())
	}
	var usage OpenAIUsage
	var firstTokenMS *int
	observer := cprnative.NewObserver(sse, cprMaxObservedEventBytes)
	observer.OnFirstOutput = func() { elapsed := int(time.Since(start).Milliseconds()); firstTokenMS = &elapsed }
	responseBound := false
	observer.OnJSON = func(data []byte) {
		if observer.HasValidatedUsage() {
			if observed, ok := extractOpenAIUsageFromJSONBytes(data); ok { usage = observed }
		}
		// Bind before exposing the terminal event to prevent a fast continuation
		// from racing the existing durable tenant-ownership write.
		if observer.Terminal != "" && observer.ResponseID != "" && observer.HasValidatedUsage() && !responseBound {
			s.bindHTTPResponseAccount(ctx, c, account, observer.ResponseID)
			responseBound = true
		}
	}
	var disconnected bool
	var copyErr, accountingErr error
	if sse {
		writeHeaders()
		c.Header("X-Accel-Buffering", "no")
		s.noteStagedOpenAICodexTurnStateCommitted(c, account, resp.Header)
		disconnected, copyErr = cprnative.Relay(c.Writer, resp.Body, observer, flush, markDisconnected)
		accountingErr = observer.Finish()
	} else {
		// JSON is a finite response. Observe before writing it so continuation
		// ownership is recorded before the client can consume response.id.
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, cprMaxObservedEventBytes+1))
		if readErr != nil || len(payload) > cprMaxObservedEventBytes {
			gap := errors.Join(errors.New("CPR JSON response incomplete or exceeds 16 MiB observation limit"), readErr)
			s.quarantineCPRAccountingGap(ctx, account, "", gap)
			return nil, cprReject(c, 502, "cpr_accounting_gap", gap.Error())
		}
		_, _ = observer.Write(payload)
		accountingErr = observer.Finish()
		writeHeaders()
		s.noteStagedOpenAICodexTurnStateCommitted(c, account, resp.Header)
		disconnected, copyErr = cprnative.Relay(c.Writer, bytes.NewReader(payload), nil, flush, markDisconnected)
	}
	if accountingErr != nil {
		if observer.Failed && !observer.SemanticOutput && len(observer.UsageJSON) == 0 {
			return nil, errors.Join(errors.New("CPR returned explicit failure without reported usage"), copyErr)
		}
		s.quarantineCPRAccountingGap(ctx, account, observer.ResponseID, accountingErr)
		if !observer.HasValidatedUsage() { return nil, errors.Join(accountingErr, copyErr) }
	}
	result := &OpenAIForwardResult{
		RequestID: resp.Header.Get("x-request-id"), ResponseID: observer.ResponseID,
		Usage: usage, Model: info.Model, BillingModel: info.Model, UpstreamModel: info.Model,
		UpstreamResponseModel: observer.Model, UpstreamResponseServiceTier: observer.ServiceTier,
		Stream: sse, OpenAIWSMode: false, UpstreamEndpoint: "/v1/responses",
		UpstreamTerminalEvent: observer.Terminal, ResponseHeaders: resp.Header.Clone(),
		UpstreamHeaders: resp.Header.Clone(), Duration: time.Since(start), FirstTokenMs: firstTokenMS,
		ClientDisconnect: disconnected || ctx.Err() != nil, ImageCount: observer.ImageCount, SearchCount: observer.SearchCount,
	}
	if info.ServiceTier != "" { result.ServiceTier = &info.ServiceTier }
	if info.Effort != "" { result.ReasoningEffort = &info.Effort; result.RequestedReasoningEffort = &info.Effort }
	if result.ImageCount > 0 {
		image, configErr := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, info.Model)
		if configErr == nil { result.ImageSize = image.SizeTier; result.ImageInputSize = image.InputSize }
	}
	if !responseBound && observer.Terminal != "" { s.bindHTTPResponseAccount(ctx, c, account, observer.ResponseID) }
	if accountingErr != nil { return result, errors.Join(accountingErr, copyErr) }
	if observer.Failed { return result, errors.Join(errors.New("CPR returned response.failed"), copyErr) }
	return result, copyErr
}
