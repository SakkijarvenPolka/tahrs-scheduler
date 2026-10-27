# tahrs-scheduler 仓库约定

本文件只记录**本仓库**的编码约定。计划与设计决策见
`/home/qiai/workspace/tahrs-scheduler-dev-plan.md`（不在仓库内）与
`docs/architecture.md`。

## 语言

- 代码、标识符、注释、日志、commit 信息：英文。
- `README.md` 与 `docs/`：中文（与计划正文一致）。
- 报错字符串：英文，`fmt.Errorf` 链路保持可读，例如
  `fmt.Errorf("config: read %q: %w", path, err)`。

## 依赖边界（计划 §3.2，评审会看）

- `internal/model`、`internal/scoring`：**禁止** import `k8s.io/*`，也不认识 Pod/Node。
- `internal/config`：只允许 `sigs.k8s.io/yaml`（不引 k8s API 类型）。
- `internal/kubeadapter`、`internal/plugin/tahrs`、`test/framework`：允许 k8s 依赖。
- 新增第三方依赖前先问一句：标准库能不能解决。

## 日志

- 只用 `klog`，结构化键值对，禁止字符串拼接。
  - 正常：`klog.InfoS("Loaded tahrs scheduler configuration", "config", fingerprint)`
  - 异常：`klog.ErrorS(err, "Scored node with fallback", "pod", klog.KObj(pod), "node", name)`
- 键名小写 camelCase；k8s 对象一律 `klog.KObj`，不要把 Pod 名拼进 message。
- Score 热路径**不打印正常日志**，只在异常/降级路径打 ErrorS，且每节点每条路径最多一行。
- 启动时必须打印一行配置指纹（`config.Loaded.Fingerprint`）。

## 错误处理

- 包装用 `%w` 并带上下文（哪个文件、哪个节点、哪个画像）。
- 可判定的语义错误用 sentinel（`model.ErrInvalidNumeric`、`model.ErrGPUNodeInsufficient`），
  调用侧用 `errors.Is` 分辨。
- 配置校验把**所有**问题聚合成 `*config.ValidationError`，不要在第一个错误处返回。
- 框架插件里：PreFilter/PreScore 可以返回 Status；**Score 永远返回 `(MinScore, nil)`**。

## 测试

- 表驱动 + 子测试名用短句（`want_...`、`anomaly_is_min_score`）。
- golden 文件放 `testdata/`，用 `-update` 重建（`go test ./internal/scoring -update`）。
- 断言用标准库 `testing`，不引入 testify 之类的新依赖。
- 单测不得访问集群或网络；需要 k8s 对象时用 `k8s.io/kubernetes/pkg/scheduler/testing`
  的 `st.MakePod()/MakeNode()`。
- 提交前跑：`gofmt -l .`、`go vet ./...`、`go test -race ./...`。

## 提交

- 一行英文摘要（`feat:`/`fix:`/`test:`/`docs:` 前缀可省略），空行，然后按模块列要点，
  末尾写验证方式（`Verified: go test -race ./...`）。

## 常用命令

```bash
go build ./cmd/tahrs-scheduler
go build -o /tmp/tahrsctl ./replay
go test -race ./...
hack/sync-replace.sh   # 同步 33 条 staging replace（改 k8s 版本后）
```
