// SPDX-License-Identifier: LGPL-3.0-only
package handler

import (
	"errors"
	"net/http"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CheckCPRIngress augments rather than replaces APIKeyAuth and model permissions.
func (h *OpenAIGatewayHandler) CheckCPRIngress(c *gin.Context) bool {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil { return true }
	subject, hasSubject := middleware2.GetAuthSubjectFromContext(c)
	userID := int64(0)
	if hasSubject { userID = subject.UserID }
	if err := h.gatewayService.PrepareCPRIngress(c, apiKey.Group, userID); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, service.ErrCPRNativeAccessDenied) { status = http.StatusForbidden }
		var maxBody *http.MaxBytesError
		if errors.As(err, &maxBody) { status = http.StatusRequestEntityTooLarge }
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
			"type": "invalid_request_error", "code": "cpr_native_ingress_rejected", "message": err.Error(),
		}})
		return false
	}
	return true
}

// TryCPRModels handles both the ordinary /models list and the Codex manifest.
func (h *OpenAIGatewayHandler) TryCPRModels(c *gin.Context) bool {
	if !service.IsCPRNativeRequest(c) { return false }
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.Group == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return true
	}
	account, err := h.gatewayService.ForwardCPRModels(c, apiKey.Group)
	if account != nil { setOpsSelectedAccount(c, account.ID, account.Platform) }
	if err != nil && !c.Writer.Written() { h.errorResponse(c, http.StatusBadGateway, "api_error", err.Error()) }
	return true
}
