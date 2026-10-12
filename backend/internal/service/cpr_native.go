// SPDX-License-Identifier: LGPL-3.0-only
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/cprnative"
	"github.com/gin-gonic/gin"
)

const (
	CPRUpstreamExtraKey = "cpr_upstream"
	cprSnapshotContextKey = "cpr_native_ingress_snapshot_v1"
	cprGroupContextKey = "cpr_native_dedicated_group_v1"
	cprMaxSnapshotBytes = 64 << 20
	cprMaxObservedEventBytes = 16 << 20
	cprMaxRequestDuration = 2 * time.Hour
	cprDisconnectDrainBudget = 15 * time.Second
)

func (a *Account) IsCPRUpstream() bool {
	if a==nil || a.Platform!=PlatformOpenAI || a.Type!=AccountTypeAPIKey { return false }
	enabled,ok:=a.Extra[CPRUpstreamExtraKey].(bool)
	return ok && enabled
}
func IsCPRNativeRequest(c *gin.Context) bool { return c!=nil && c.GetBool(cprGroupContextKey) }

func cprEndpoint(path string) string {
	path=strings.TrimRight(path,"/")
	for _,prefix:=range []string{"/v1","/openai/v1","/backend-api/codex",""} {
		for _,endpoint:=range []string{"responses","models"} { if path==prefix+"/"+endpoint { return endpoint } }
	}
	return ""
}

type cprIngressSnapshot struct {
	Header http.Header
	Path string
	RawQuery string
	Capture cprnative.Capture
	Complete bool
	ReadError error
}
type cprCapturingBody struct { io.ReadCloser; snapshot *cprIngressSnapshot }
func (b *cprCapturingBody) Read(p []byte) (int,error) {
	n,err:=b.ReadCloser.Read(p)
	if n>0 { _,_=b.snapshot.Capture.Write(p[:n]) }
	if err==io.EOF { b.snapshot.Complete=true } else if err!=nil { b.snapshot.ReadError=err }
	return n,err
}

// Register before route-local request-ID/model/compatibility middleware.
// Reads still pass through the host body limiter; authentication is unchanged.
func CPRIngressSnapshotMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		endpoint:=cprEndpoint(c.Request.URL.Path)
		if endpoint=="" { c.Next(); return }
		snapshot:=&cprIngressSnapshot{Header:c.Request.Header.Clone(),Path:c.Request.URL.Path,RawQuery:c.Request.URL.RawQuery,Capture:cprnative.Capture{Limit:cprMaxSnapshotBytes}}
		if endpoint=="responses" && c.Request.Method==http.MethodPost && c.Request.Body!=nil {
			c.Request.Body=&cprCapturingBody{ReadCloser:c.Request.Body,snapshot:snapshot}
		} else { snapshot.Complete=true }
		c.Set(cprSnapshotContextKey,snapshot); c.Next()
	}
}
func cprSnapshot(c *gin.Context) (*cprIngressSnapshot,error) {
	if c==nil { return nil,errors.New("CPR native mode requires the gateway ingress") }
	v,exists:=c.Get(cprSnapshotContextKey); snap,ok:=v.(*cprIngressSnapshot)
	if !exists || !ok || snap==nil { return nil,errors.New("original CPR snapshot unavailable; refusing rewritten fallback") }
	return snap,nil
}

// This preview deliberately supports one trusted sub2api owner per native group.
// It does not silently discard namespace-based isolation for unrelated tenants.
func (a *Account) CPRNativeOwnerUserID() int64 {
	if a==nil { return 0 }
	switch value:=a.Extra["cpr_owner_user_id"].(type) {
	case int: if value>0 { return int64(value) }
	case int64: if value>0 { return value }
	case float64: if value>0 && value<=9007199254740991 && value==math.Trunc(value) { return int64(value) }
	case json.Number: if id,err:=value.Int64(); err==nil && id>0 { return id }
	}
	return 0
}

var ErrCPRNativeAccessDenied=errors.New("此CPR原生分组只允许管理员指定的sub2api用户")

func (s *OpenAIGatewayService) CPRGroupMode(ctx context.Context,group *Group) (bool,int64,error) {
	if group==nil || group.Platform!=PlatformOpenAI { return false,0,nil }
	if s==nil || s.accountRepo==nil { return false,0,errors.New("account repository unavailable") }
	members,err:=s.accountRepo.ListByGroup(ctx,group.ID)
	if err!=nil { return false,0,errors.New("CPR group account lookup failed") }
	return cprGroupMembersMode(members)
}
func cprGroupMembersMode(members []Account) (bool,int64,error) {
	marked:=false
	for i:=range members { if members[i].IsCPRUpstream() { marked=true; break } }
	if !marked { return false,0,nil }
	ownerID:=int64(0)
	for i:=range members {
		if !members[i].IsCPRUpstream() { return true,0,errors.New("CPR原生分组不能混入普通账号；请使用独立OpenAI分组") }
		owner:=members[i].CPRNativeOwnerUserID()
		if owner<=0 { return true,0,errors.New("CPR账号缺少有效的cpr_owner_user_id") }
		if ownerID!=0 && ownerID!=owner { return true,0,errors.New("同一CPR原生分组的账号必须绑定同一个sub2api用户") }
		ownerID=owner
	}
	return true,ownerID,nil
}

// Runs after authentication and before permissions/routing/body normalization.
func (s *OpenAIGatewayService) PrepareCPRIngress(c *gin.Context,group *Group,userID int64) error {
	enabled,ownerID,err:=s.CPRGroupMode(c.Request.Context(),group)
	if err!=nil { return err }; if !enabled { return nil }
	if userID<=0 || userID!=ownerID { return ErrCPRNativeAccessDenied }
	c.Set("cpr_native_owner_user_id",userID); c.Set(cprGroupContextKey,true)
	endpoint:=cprEndpoint(c.Request.URL.Path)
	if (endpoint=="responses" && c.Request.Method==http.MethodPost) || (endpoint=="models" && c.Request.Method==http.MethodGet) {
		snapshot,err:=cprSnapshot(c); if err!=nil { return err }
		if endpoint=="models" { return nil }
		if encoding:=strings.TrimSpace(snapshot.Header.Get("Content-Encoding")); encoding!="" && !strings.EqualFold(encoding,"identity") { return errors.New("CPR原生入口暂不接受编码后的请求正文，请发送UTF-8 JSON") }
		if !snapshot.Complete {
			if c.Request.Body==nil { return errors.New("missing CPR request body") }
			if _,err:=io.Copy(io.Discard,c.Request.Body); err!=nil { return err }
		}
		if !snapshot.Complete || snapshot.ReadError!=nil { return errors.New("incomplete original CPR request body") }
		if snapshot.Capture.Overflow { return errors.New("CPR original request exceeds 64 MiB snapshot limit") }
		if _,err:=cprnative.InspectRequest(snapshot.Capture.Bytes); err!=nil { return err }
		_ = c.Request.Body.Close()
		c.Request.Body=io.NopCloser(bytes.NewReader(snapshot.Capture.Bytes))
		c.Request.ContentLength=int64(len(snapshot.Capture.Bytes))
		return nil
	}
	if c.Request.Method==http.MethodGet && strings.HasSuffix(c.Request.URL.Path,"/usage") { return nil }
	return errors.New("CPR原生模式本版仅开放HTTP POST /responses和GET /models；请关闭WebSocket，不会隐式桥接")
}

// Local affinity only. Never modify a header/body transmitted to CPR.
func CPRNativeRoutingKey(c *gin.Context,userID int64,fallback string) string {
	snapshot,err:=cprSnapshot(c); if err!=nil { return fallback }
	for _,name:=range []string{"session-id","session_id","thread-id","conversation_id"} {
		if value:=snapshot.Header.Get(name); value!="" {
			hash:=sha256.Sum256([]byte(fmt.Sprintf("cpr:%d:%s:%s",userID,name,value)))
			return fmt.Sprintf("cpr:%x",hash[:])
		}
	}
	return fallback
}
func cprReject(c *gin.Context,status int,code,message string) error {
	if c!=nil && !c.Writer.Written() { c.AbortWithStatusJSON(status,gin.H{"error":gin.H{"type":"invalid_request_error","code":code,"message":message}}) }
	return errors.New(message)
}
func (s *OpenAIGatewayService) quarantineCPRAccountingGap(ctx context.Context,account *Account,responseID string,reason error) {
	durableCtx,cancel:=context.WithTimeout(context.WithoutCancel(ctx),5*time.Second); defer cancel()
	message:="CPR_NATIVE_ACCOUNTING_GAP: usage could not be validated; reconcile CPR logs before clearing account error"
	err:=s.accountRepo.SetError(durableCtx,account.ID,message)
	slog.ErrorContext(durableCtx,"cpr_native_accounting_gap","account_id",account.ID,"response_id",responseID,"reason",reason,"quarantine_error",err)
}
