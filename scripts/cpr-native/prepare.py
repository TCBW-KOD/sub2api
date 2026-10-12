#!/usr/bin/env python3
"""Materialize audited CPR integration hooks before building this branch.

No network, credentials, GitHub writes or fuzzy patching. Docker invokes this
step automatically; a native source build must run it first. Every input blob
and anchor is verified before writing. Changed upstream versions fail closed.
"""
from __future__ import annotations
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

BASE = '3a6fd1c9db07203ca308aaba69e502bc1f35b307'
BLOBS = {
    'backend/internal/service/openai_gateway_forward.go': '16df53310f6aff35da90f494f89fda17a56c5937',
    'backend/internal/handler/openai_gateway_handler.go': '65d002e54b60b6eedb19bfc5b514ae1d7ca8b9b8',
    'backend/internal/handler/openai_codex_models_handler.go': '777eeebdb15aed4fa503705c973fd26906b4a691',
    'backend/internal/server/routes/gateway.go': 'a921a42d44a135b0fd417a1c6664c046ff83fec3',
    'frontend/src/views/admin/AccountsView.vue': '9b9a4b59a1f0b2e5915308e548069fe05b4b39ce',
}
REQUIRED = (
    'backend/internal/pkg/cprnative/request.go',
    'backend/internal/pkg/cprnative/observer.go',
    'backend/internal/pkg/cprnative/catalog.go',
    'backend/internal/service/cpr_native.go',
    'backend/internal/service/cpr_native_forward.go',
    'backend/internal/service/cpr_native_models.go',
    'backend/internal/handler/cpr_native_handler.go',
    'frontend/src/components/account/CPRUpstreamEntry.vue',
)
MARKER = '.cpr-native-integration.json'


def blob_sha(data: bytes) -> str:
    return hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()


def replace_once(source: str, before: str, after: str, label: str) -> str:
    count = source.count(before)
    if count != 1:
        raise ValueError(f'{label}: expected one anchor, found {count}; refusing fuzzy integration')
    return source.replace(before, after, 1)


def transform(path: str, source: str) -> str:
    def one(before: str, after: str, label: str) -> None:
        nonlocal source
        source = replace_once(source, before, after, path + ':' + label)

    if path.endswith('/openai_gateway_forward.go'):
        sig = 'func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {\n'
        one(sig, sig + '''\t// CPR must branch before every compatibility transform.
\tif IsCPRNativeRequest(c) && (account == nil || !account.IsCPRUpstream()) {
\t\treturn nil, cprReject(c, 409, "cpr_native_account_changed", "CPR原生请求不得回退至普通账号")
\t}
\tif account != nil && account.IsCPRUpstream() {
\t\treturn s.forwardCPRNative(ctx, c, account, body)
\t}
''', 'native-before-transform')
    elif path.endswith('/openai_gateway_handler.go'):
        start = source.index('func (h *OpenAIGatewayHandler) Responses(c *gin.Context) {')
        end = source.find('\nfunc ', start + 1)
        if end == -1:
            end = len(source)
        prefix, section, suffix = source[:start], source[start:end], source[end:]
        source = section
        one('''\tbody, ok = h.normalizeOpenAIResponsesCompactRequest(c, reqLog, body)
\tif !ok {
\t\treturn
\t}
''', '''\tif !service.IsCPRNativeRequest(c) {
\t\tbody, ok = h.normalizeOpenAIResponsesCompactRequest(c, reqLog, body)
\t\tif !ok { return }
\t}
''', 'preserve-native-compaction')
        one('''\tstopCompactKeepalive := service.StartOpenAICompactSSEKeepalive(c, h.openAICompactKeepaliveInterval())''', '''\tstopCompactKeepalive := func() {}
\tif !service.IsCPRNativeRequest(c) {
\t\tstopCompactKeepalive = service.StartOpenAICompactSSEKeepalive(c, h.openAICompactKeepaliveInterval())
\t}''', 'no-synthetic-heartbeats')
        one('''\t} else if changed {
\t\tbody = cappedBody
\t}''', '''\t} else if changed {
\t\tif service.IsCPRNativeRequest(c) {
\t\t\th.errorResponse(c, http.StatusConflict, "cpr_native_policy_conflict", "推理策略要求改写请求；请取消该改写策略")
\t\t\treturn
\t\t}
\t\tbody = cappedBody
\t}''', 'reject-reasoning-override')
        for name in ('normalizeCodexAutomationBootstrap', 'normalizeCodexDelegationBootstrap'):
            one(f'if normalizedBody, changed := {name}(body); changed {{',
                f'if normalizedBody, changed := {name}(body); changed && !service.IsCPRNativeRequest(c) {{', name)
        one('''\tchannelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
''', '''\tchannelMapping, channelErr := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
\tif service.IsCPRNativeRequest(c) {
\t\tif channelErr != nil {
\t\t\th.errorResponse(c, http.StatusForbidden, "cpr_native_channel_denied", "渠道模型权限校验失败")
\t\t\treturn
\t\t}
\t\tif channelMapping.Mapped && channelMapping.MappedModel != reqModel {
\t\t\th.errorResponse(c, http.StatusConflict, "cpr_native_policy_conflict", "渠道模型映射与CPR原生模式冲突")
\t\t\treturn
\t\t}
\t}
''', 'reject-channel-override')
        one('if !h.validateFunctionCallOutputRequest(c, body, reqLog) {',
            'if !service.IsCPRNativeRequest(c) && !h.validateFunctionCallOutputRequest(c, body, reqLog) {',
            'delegate-tool-validation')
        one('h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)',
            'h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream && !service.IsCPRNativeRequest(c), &streamStarted, reqLog)',
            'no-user-queue-sse-injection')
        one('''\tsessionHash := h.gatewayService.GenerateSessionHash(c, sessionHashBody)
''', '''\tsessionHash := h.gatewayService.GenerateSessionHash(c, sessionHashBody)
\tif service.IsCPRNativeRequest(c) {
\t\tsessionHash = service.CPRNativeRoutingKey(c, subject.UserID, sessionHash)
\t}
''', 'local-affinity-without-wire-rewrite')
        one('h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)',
            'h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream && !service.IsCPRNativeRequest(c), &streamStarted, reqLog)',
            'no-account-queue-sse-injection')
        one('''\t\tattemptBody := h.deriveOpenAIForwardAttemptBody(reqLog, forwardBody, account, &passthroughFailoverState)''', '''\t\tattemptBody := forwardBody
\t\tif !service.IsCPRNativeRequest(c) {
\t\t\tattemptBody = h.deriveOpenAIForwardAttemptBody(reqLog, forwardBody, account, &passthroughFailoverState)
\t\t}''', 'no-replay-adaptation')
        source = prefix + source + suffix
    elif path.endswith('/openai_codex_models_handler.go'):
        one('''\tifNoneMatch := c.GetHeader("If-None-Match")''', '''\tif h.TryCPRModels(c) { return }
\tifNoneMatch := c.GetHeader("If-None-Match")''', 'native-manifest')
    elif path.endswith('/routes/gateway.go'):
        one('''\tbodyLimit := middleware.RequestBodyLimit(cfg.Gateway.MaxBodySize)''', '''\tr.Use(service.CPRIngressSnapshotMiddleware())
\tbodyLimit := middleware.RequestBodyLimit(cfg.Gateway.MaxBodySize)''', 'capture-original')
        one('''\tgroupModelAllowlist := middleware.GroupModelAllowlist()''', '''\toriginalGroupModelAllowlist := middleware.GroupModelAllowlist()
\tgroupModelAllowlist := func(c *gin.Context) {
\t\tif !h.OpenAIGateway.CheckCPRIngress(c) { return }
\t\toriginalGroupModelAllowlist(c)
\t}''', 'retain-auth-and-model-guard')
        one('''\tmodelsHandler := func(c *gin.Context) {
''', '''\tmodelsHandler := func(c *gin.Context) {
\t\tif h.OpenAIGateway.TryCPRModels(c) { return }
''', 'native-ordinary-models')
    elif path.endswith('/AccountsView.vue'):
        one('<script setup lang="ts">', '<script setup lang="ts">\nimport CPRUpstreamEntry from \'@/components/account/CPRUpstreamEntry.vue\'\n', 'ui-import')
        one('''            <template #after>
''', '''            <template #after>
              <CPRUpstreamEntry :groups="groups" @saved="handleManualRefresh" />
''', 'ui-entry')
    else:
        raise ValueError('Unknown integration path: ' + path)
    return source


def prepare(repo: Path, check: bool = False) -> None:
    for name in REQUIRED:
        if not (repo / name).is_file():
            raise ValueError(f'Missing CPR source: {name}')
    marker = repo / MARKER
    if marker.exists():
        state = json.loads(marker.read_text(encoding='utf-8'))
        if state.get('base') != BASE or set(state.get('files', {})) != set(BLOBS):
            raise ValueError('Invalid integration marker')
        for name, digest in state['files'].items():
            if hashlib.sha256((repo / name).read_bytes()).hexdigest() != digest:
                raise ValueError(f'Integrated source changed: {name}; review before rebuilding')
        print('CPR integration already present and verified (not a build/test result).')
        return
    if shutil.which('gofmt') is None:
        raise ValueError('Go/gofmt is required for integration')
    pending: dict[str, bytes] = {}
    for name, expected in BLOBS.items():
        raw = (repo / name).read_bytes()
        if blob_sha(raw) != expected:
            raise ValueError(f'Upstream blob mismatch: {name}; expected baseline {BASE}')
        out = transform(name, raw.decode('utf-8')).encode('utf-8')
        if name.endswith('.go'):
            out = subprocess.run(['gofmt'], input=out, check=True, capture_output=True).stdout
        pending[name] = out
    if check:
        print('All pinned source hashes and integration anchors verified. No files changed.')
        return
    for name, out in pending.items():
        target = repo / name
        tmp = target.with_name(target.name + '.cpr-tmp')
        tmp.write_bytes(out)
        tmp.replace(target)
    marker.write_text(json.dumps({
        'base': BASE, 'feature': 'cpr-native-http-preview',
        'files': {name: hashlib.sha256(out).hexdigest() for name, out in pending.items()},
        'status': 'integration-applied-not-production-validated',
    }, indent=2) + '\n', encoding='utf-8')
    print('CPR hooks integrated into backend routes/forwarder and account UI.')
    print('Run complete build and billing tests before deployment.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    try:
        prepare(args.repo.resolve(), args.check)
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        parser.exit(1, f'ERROR: {exc}\n')
