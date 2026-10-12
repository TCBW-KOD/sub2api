# CPR 原生转发测试状态

这是开发预览的真实测试记录，不是生产批准。

## 已实际执行

标准库核心在本地隔离临时模块中运行，使用 Go 1.23.2：

```bash
GOTOOLCHAIN=local GOPROXY=off go test -race -count=1 -v ./...
```

- 31 个顶层 `TestCPR...` 测试通过。
- `FuzzCPRValidateJSON` 的 5 个种子作为普通测试通过；没有持续 fuzz 测试。
- race 检查没有报告数据竞争。
- Mock HTTP 测试实际通过 `httptest` 服务端捕获请求正文/头，并比较原字节；不是只比较自己构造的两个字符串。
- 测试不访问 OpenAI、真实 CPR、实际 OAuth 账号、PostgreSQL 或 Redis，不产生用户模型费用。

完整输出：[core-tests.txt](core-tests.txt)。同一套测试可由 `scripts/cpr-native/test-core.sh` 复制实际核心到临时模块后重跑。临时 `go.mod` 不属于仓库源码，不在 `backend/internal/pkg/cprnative` 建立嵌套模块。

## 被测试文件与已提交文件一致性

以下 Git blob SHA 由本地原始文件计算，并已与 GitHub contents API 返回值逐一核对：

| 文件（backend/internal/pkg/cprnative/） | Git blob SHA |
|---|---|
| request.go | `55ac46a809f3dfb7a29481a148868cebb1e59d80` |
| observer.go | `8fc8205b29259eccb114824690addfe84125affd` |
| catalog.go | `2b23aed5f0533a81c10153bcacbb19d250fd77d7` |
| native_test.go | `6170b28206ff553ea410683ef78092f5025ad9f0` |
| safety_regression_test.go | `085cc27a34224528ac0a0fb08345b0064aedab8e` |

核心覆盖：body/grammar/AdditionalTools/多模态结果保留，原始动态会话头、不同别名、未知业务头，认证和逐跳头边界，严格JSON与策略冲突，SSE任意分块/多行/CRLF，大事件有界观察，真实累计usage和缓存计量，重复事件不叠加，断流/取消计量，响应ID冲突，模型目录Lite能力保留，模型权限过滤，POST传输自动重放防护。

## 已提交但尚未运行

- `backend/internal/service/cpr_native_test.go`：6项宿主服务/Gin接入测试。
- `frontend/src/components/account/__tests__/CPRUpstreamEntry.spec.ts`：6项UI/API交互测试。
- `scripts/cpr-native/validate.sh`：固定源码临时检出、准备接线、前端类型检查/构建、宿主服务测试、handler/routes测试编译、嵌入前端的完整后端构建。

## 尚未验证，不能算通过

- 五处宿主接线在完整检出上的实际应用及全宿主Go类型检查/链接。
- Vue完整类型检查和实际浏览器交互。
- Docker镜像构建或任何部署。
- PostgreSQL/Redis真实计费、并发余额预留、订阅额度和释放行为。
- 真实Codex Desktop -> sub2api -> CPR多轮工具/续接/压缩。
- 原生WebSocket：本版明确未实现，不属于通过范围。
- 不同不可信用户共享CPR状态池：本版以单个可信用户的专用分组约束，不声称支持。

当前运行环境无法通过git下载完整仓库，Go版本也低于上游要求；GitHub源码读写正常，功能源码已提交。检索时此开发分支没有GitHub Actions运行记录，不能将“已提交”当作“CI绿色”。

`validate.sh` 若成功仍仅给出构建和选定单元测试结论；真实CPR与数据库计费验收必须另行完成。发生 `CPR_NATIVE_ACCOUNTING_GAP` 需要人工核对，不能标为零费成功或未经核对直接反复清错。
