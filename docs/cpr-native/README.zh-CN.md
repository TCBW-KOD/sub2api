# OpenAI · CPR上游（HTTP/SSE 开发预览）

本分支：`TCBW-KOD/sub2api` 的 `feat/cpr-upstream`。
上游基线：`Wei-Shaw/sub2api` v0.2.15 / `3a6fd1c9db07203ca308aaba69e502bc1f35b307`。

仓库名、Description、上游 LICENSE 和原 README 保持不变。`main` 不包含本功能；请明确选择开发分支。本功能不是插件，也不是已经完成生产验收的正式发行版。

## 目标与边界

```
Codex Desktop（HTTP/SSE）
  -> sub2api：鉴权、权限、审计、余额/额度、并发、账号选择、用量计费
  -> CPR：原始 Codex 业务请求、工具协议、最终上游账号和协议适配
```

只有管理员明确标记 `extra.cpr_upstream=true` 的 OpenAI API Key 账号进入新路径。原始业务正文和动态会话头在兼容改写前保存，CPR 路径不执行 custom-to-function、grammar 删除、namespace 摊平、Lite 能力覆盖或模型别名改写。合法未知业务字段不依赖枚举白名单。

“原始”指业务正文与获准的端到端业务头值，不是 TLS、HTTP 分块、头名称大小写或网络数据包相同。认证、Host、逐跳头、Cookie、代理环境头、压缩协商和必要安全边界仍按代理规则处理。不会继承客户端访问 sub2api 的密钥作为 CPR 上游凭据。

**当前限制必须接受：**

- 只实现 HTTP POST `/responses` 和 GET `/models`（含 Codex 能力目录及受支持别名）。不支持原生 WebSocket、Chat Completions、旧独立 `/responses/compact` 或独立 Images/Search/Live。请求正文中的 Codex 工具和原生 Responses 压缩内容不因而被删除。未支持入口明确拒绝，不隐式走旧路径。
- 每个 CPR 专用 OpenAI 分组必须只包含 CPR 账号，且绑定同一个管理员指定的 **sub2api 用户 ID**。该用户多个 API Key 可用。不是 GitHub ID。这是本预览的可信用户边界，不声称已证明互不信任用户共享原始会话状态的安全性。
- 子账号轮换和协议恢复交给 CPR。CPR POST 已发出后的不确定错误不自动重放，也不偷偷换成普通账号。
- 不承诺账号不可识别、风控规避、缓存命中率或所有 Codex Desktop 功能可用。

## 构建接入方式：不要漏掉准备步骤

新增功能源码位于正常的 `backend/internal/...` 和 `frontend/src/...` 目录。

为保持本次原版接入点可核查，五个原版大文件的接线由 **`scripts/cpr-native/prepare.py` 在构建前生成**，不是已经直接覆写到 Git 中。这五处是 Forward、Responses handler、CodexModels handler、网关路由、AccountsView。

脚本先验证五个原版文件的 Git blob 哈希和唯一插入点，全部匹配后才写入接线。它不做模糊套用，不联网、不获取凭据、不推送仓库。重复运行通过 `.cpr-native-integration.json` 校验已生成文件。升级上游需要重新审阅接入点，不能删除校验强行套用。

**本分支 Dockerfile 已自动执行准备步骤。原生 Go/Vue 构建必须先执行该脚本。** 否则新源码虽存在，账号页面和转发入口不会接通。

### 本地验证

在新的开发目录检出本分支，不使用生产配置或数据库：

```bash
git clone --branch feat/cpr-upstream https://github.com/TCBW-KOD/sub2api.git
cd sub2api

# 标准库核心测试，无需下载完整宿主依赖；至少 Go 1.23 和 race 所需工具链。
bash scripts/cpr-native/test-core.sh

# 在干净提交上导出临时检出，执行接线、前后端测试和完整构建。
# 需要 Python 3、与 backend/go.mod 匹配的 Go、Node、pnpm 9。
bash scripts/cpr-native/validate.sh
```

`validate.sh` 不覆盖当前工作区；它只从 Git HEAD 导出已跟踪源码到临时目录。日志与产物保留在打印的本地目录，不向 GitHub 或模型上游上传。脚本成功仅代表构建和选定单元测试，不代表真实计费/CPR端到端验收。

普通源码开发可先执行：

```bash
python3 scripts/cpr-native/prepare.py
```

该命令会修改五个集成文件并生成校验标记。不要在已作本地改动的原版大文件上强行运行。Docker 构建在新阶段内独立生成接线，避免把“有新文件但没接通”构建成镜像。

### Docker 测试镜像

```bash
docker build --build-arg VERSION=0.2.15-cpr-native-preview -t sub2api-cpr:preview .
```

本次没有实际构建该镜像，命令不是构建成功记录。测试部署使用单独的 PostgreSQL、Redis 和数据卷，不连接生产数据库，不直接覆盖原服务。

## 账号页面使用

通过隔离构建与验收后：

1. 新建空的 OpenAI 专用分组，设置所需模型访问权限和价格，不配置会改写正文的模型别名、推理或 Fast 策略。
2. 进入“账号管理”，点击 **CPR上游**。选择新增或启用已有 OpenAI API Key 账号。
3. 填入 CPR `/v1` Base URL、CPR 客户端 Key、并发和倍率、专用分组，以及允许使用的 sub2api 用户 ID。启用已有账号不会重发其凭据，但会替换为所选分组绑定。
4. 使用该用户绑定此分组的 sub2api Key 接入 Codex Desktop，明确关闭 WebSocket，使用 HTTP/SSE Responses。

“CPR上游”采用独立标记，不依赖旧的“自动透传（仅替换认证）”。UI 写入强制 Responses、WS off、不注入 Hosted 工具等辅助设置；实际转发仍由原生分支和后端校验决定。

遇到模型/推理/Fast 策略要求改写，返回明确的策略冲突而非忽略权限。分组模型白名单仍然生效；目录只删除无权看到的模型，不篡改获准模型的能力字段。固定目录来源时只允许一个本组 CPR 账号，不能混合多个提供商清单。

## 计费、取消与异常

返回实体和计量观察分离。SSE 按原字节转发，JSON 有界读取后不重写输出；用量通过原有 OpenAIForwardResult/RecordUsage 流程入账。缓存 token 使用实际返回计量，重复累计事件不相加。POST 的自动传输重放被禁止。

客户端断开后最多等待15秒收集已执行请求的用量；整体请求最长2小时。JSON 计量上限16MiB、模型目录8MiB、请求快照64MiB，仍受宿主更小限制约束。

缺失或非法用量不会被伪造为零费成功：记录 `CPR_NATIVE_ACCOUNTING_GAP`，并将 CPR 入口设为错误状态。合法已知部分用量仍可入账；未知部分需管理员与 CPR 日志对账。此机制没有自动补账队列，也不是数据库 exactly-once 的证明。

## 验证状态与验收门槛

见 [TEST_STATUS.md](TEST_STATUS.md) 和 [core-tests.txt](core-tests.txt)。

已执行标准库核心31项测试和5个普通 fuzz 种子，包含 race 检查。宿主服务6项测试和UI6项测试已提交但尚未执行；全宿主编译、Docker、真实 CPR、余额扣减/订阅额度、取消释放和负载测试未完成。

上线前至少验证：直连CPR与经过本分支的业务正文/会话头一致；无权限请求不会出站；多轮工具与续接正确；正常/失败/断流请求计费一次且并发名额释放；缺失用量暂停并可对账；普通账号回归不受影响。

当前是源码开发分支，不要将核心测试通过等同于生产验证。取消 CPR 标记会退出保真契约；回滚前暂停原生账号，防止旧版本忽略该标记后默默转入兼容路径。
