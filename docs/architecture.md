# 设计决策

这份文档按「为什么这样做」组织，而不是按「做了什么」组织。每条决策都给出上游源码位置或本仓库里的测试名，
数字都是本机实测值（2026-10-28，Go 1.27.1，k8s v0.36.5，k3s v1.36.5+k3s1）。

## 1. 为什么是 Framework 插件，而不是 Extender 或自写调度器

kube-scheduler 的 Framework 插件跑在调度器进程内，每个调度周期拿到的是**同一份** `SharedLister` 快照
（`k8s.io/kube-scheduler@v0.36.5/framework/interface.go:524` 起的 `PreFilter`/`PreScore`/`Score` 都接收 `CycleState`），
所以：

- 没有网络跳转和序列化开销，也不需要自己维护一份节点缓存；
- 同一周期内的 PreFilter 与 Score 看到的是同一个快照，不会出现「PreFilter 判断装得下、Score 时快照已过期」；
- 直接复用队列、退避、事件记录、抢占与 DRA 准入流程，这些如果自写调度器都要重新实现一遍，且很难与上游行为一致。

Extender 的问题正好相反：它通过 HTTP 调用外部进程，拿不到 `CycleState`，只能每次把 Pod 和节点列表发过去；
上游文档也明确 Extender 已进入维护状态，且对一致性的保证更弱。自写完整调度器则要复制 `schedule_one.go` 的全套状态机
（未调度队列、backoff、assume/bind、抢占），收益却只是「不用改 kube-scheduler 配置」。

结论：插件形态能用最小的代码量拿到集群的真实账本（`NodeInfo`）与调度顺序保证。

## 2. 为什么画像校验放在 PreFilter（F13）

上游在 `pkg/scheduler/schedule_one.go` 的 `scheduleOne` 里对可行节点做过短路：**只剩一个可行节点时不调用 PreScore/Score**。
本机验证（`deploy/README.md` 的链路验证记录）：单节点集群里，`evaluatedNodes=1 feasibleNodes=1` 的调度
日志中 `Scored node` 计数为 **0**——插件根本没被调用。

如果画像校验写在 Score 里，那么在「只有一个节点能放下这个 Pod」的情况下，未知画像、非法注记都会被静默忽略，
Pod 照常被绑定，配置错误永远不会暴露。所以：

- 画像解析、未知画像拒绝、资源账本计算，全部放在 `PreFilter`（`internal/plugin/tahrs/prefilter.go`）；
- `PreScore` 只做一件事：确认 `CycleState` 里有 PreFilter 写下的画像与请求（`internal/plugin/tahrs/state.go`），
  缺失视为框架配置错误；
- `Score` 只读状态、纯计算。

验收用例 `test/framework/acceptance_test.go` 的 `TestUnknownProfileRejectedInPreFilter` 覆盖了 1 节点与 3 节点两种情形，
断言 `Reason` 里带配置指纹。

## 3. 资源账本口径

插件不自己遍历 Pod 的容器，而是直接问框架：`fwk.NodeInfo.GetRequested()` 返回该节点上所有 Pod 的请求之和。
这样做的原因是「账本只能有一份」——如果插件自己再算一遍，任何一次上游口径变化（例如 sidecar 的引入）都会让两边分叉。

但 `GetRequested()` 与 `GetNonZeroRequested()` 不同，必须清楚用哪个：

- `GetNonZeroRequested()`（`k8s.io/kube-scheduler@v0.36.5/framework/types.go:267-271`）跳过 CPU/内存请求为 0 的 Pod 计数，
  它是给 `NodeResourcesMostAllocated`/`LeastAllocated` 这类「按请求比例」打分用的；
- `GetRequested()` 返回真实总和，包含零请求 Pod（它们照样占 `pods` 名额与 overhead），是我们的账本口径。

`NodeInfo.AddPod` → `CalculateResource` 用的是 `component-helpers` 的 `resource.PodRequests(pod, options)`
（`k8s.io/component-helpers@v0.36.5/resource/helpers.go:151`），其口径与我们的适配器必须逐字节一致，
所以采信关系反过来：**适配器对齐上游，而不是自己发明规则**。`internal/kubeadapter/pod.go` 复刻了上游三处容易漏的行为：

1. **init 容器取 max**：普通 init 容器串行执行，所以按 `max(所有普通容器之和, 单个 init 容器)` 计；上游 `AggregateContainerRequests` 已实现；
2. **restartable init（sidecar）累加**：`restartPolicy: Always` 的 init 容器与主容器同时运行，按求和计；
3. **overhead 追加**：`pod.Spec.Overhead` 直接加到总和上（运行时类的开销，例如沙箱）。

另外两处是我们**主动选择**的保守规则（单测 `internal/kubeadapter/kubeadapter_test.go` 有对照断言）：

- **GPU 只写 limits**：apiserver 在 `pkg/apis/core/v1/defaults.go` 的 `SetDefaults_Pod` 里会把「有 limits 没有 requests」的
  资源回填成 requests 相等，所以一般情况下上游账本已经算到了；但如果对象绕过默认化（测试里直接构造、或某些控制器写入），
  上游会记 0，而适配器仍按 limits 计。我们用 DeepCopy 后的副本回填，保证两种来源结果一致（`TestGPULimitsOnlyWithoutDefaulting`）。
- **GPU request < limit**：扩展资源不可压缩，上游按 requests 记账；我们取 `max(requests, limits)`，即按 limit 预留
  （`TestGPURequestBelowLimitIsReservedAtLimit` 里上游 1 卡、我们 2 卡）。这是故意的：预留不足导致的「以为还能再塞一个」
  比浪费更难受。这一点写进文档而不是隐藏。

`metrics`、`cpu`、`memory` 用 `GetMilliCPU()`/`GetMemory()`；扩展资源用 `GetScalarResources()[gpuResourceName]`
（`framework/types.go:496` 的 `Resource` 接口）。

## 4. 为什么 Score 不能返回 Error（R7）

框架里 `Score` 返回 `*Status`，只有 `Code() == Success` 才算正常。返回 `Error` 的后果不是「这个节点扣分」，
而是**整个 Pod 的这次调度尝试失败并进入退避队列**（`schedule_one.go` 对 status 的处理），日志里是一条 error 级别的事件。
也就是说，一个配置错误（哪怕只影响一个节点）会让所有 Pod 反复失败，而不是仅仅绕开那个节点。

所以 `internal/plugin/tahrs/score.go` 的约定是：

- `scoring.Evaluate` 返回错误（例如 GPU Pod 撞到没有 GPU 的节点、数值异常）→ 该节点给 `fwk.MinScore`，同时
  打 error 日志、把计数打到 `tahrs_invalid_numeric_total{kind=...}`，`Status` 返回 `nil`；
- `CycleState` 缺失（框架问题，不是节点问题）同样降级为 `MinScore`，不阻断调度。

验收用例 `TestAnomalousNodeGetsMinScore` 与 `internal/plugin/tahrs/plugin_test.go` 的 `TestScoreAnomalyReturnsMinScore`
都断言「有异常节点时 Pod 照样能调度到其他节点」。

## 5. 评分公式：余弦为什么失效（F11）→ Bray-Curtis → 与 MostAllocated 的关系（F12）

设请求向量 `q`、节点剩余向量 `f`（allocatable 减已用），维度权重 `w`。

**余弦**：`Σ w·q·f / (|q||f|)`。失效原因有两个，都在实现阶段撞到：

- 它只看方向不看大小：请求正好是节点剩余的一半、和完全相同，夹角都是 0，得分都接近 1，无法区分「刚好装下」和「只剩一半」；
- 任何一维为 0 时点积为 0，分数直接掉到 0，一个纯 CPU Pod 落到「CPU 充足、GPU 空闲」的节点反而可能拿低分。

**Bray-Curtis 形式的 fit**：`1 − Σ w|f−t| / Σ w(f+t)`，即「剩余量和请求量的对称差，按总量归一」。
它满足我们需要的三个性质：剩余与请求越接近越接近 1；两维都放大不改变分数（尺度无关）；某一维为 0 时只按该维权重计入偏差。
`q` 在某一维为 0（例如 CPU Pod 的 GPU 维）直接跳过该维，避免「请求 0 个 GPU 就等于最不合适」。

**pack**：`Σ w·(used) / Σ w·(allocatable)`，即 MostAllocated 方向——已用比例越高越受欢迎，用于减少碎片。
`used` 取节点当前已用，不含本次请求；超过 allocatable（超卖节点）时 floor 到 0。

**F12 的诚实结论**：pack 与上游 `MostAllocated` 同向，但 fit 与 pack 并不总是同向。单测里的分歧用例
（`internal/scoring/evaluate_test.go` 的 `TestFitAndPackDisagree`）：请求 `q=(500m, 1Gi)`，

- 节点 A：allocatable `(4000m, 4Gi)`，剩余空间大、形状更接近请求，`fit=0.3158`；
- 节点 B：allocatable `(600m, 16Gi)`，CPU 几乎被请求本身耗尽，形状很差，`fit=0.1657`；
- 但 A 的已用比例低，`pack=0.1851`，B 的 CPU 已用比例高，`pack=0.3267`；
- 于是 `α=1, β=0` 时 A > B，`α=0, β=1` 时排序翻转。

所以「tahrs 比 MostAllocated 更好」这种说法不成立：它只是多了一个可调的维度。GPU 偏好这一项没有放进连续权重，
而是用 `gpuNodePolicy` 加一个常数罚分（默认 30）——它的语义是「CPU 负载不该被放到 GPU 池里」的硬偏好，
平滑的权重表达不了这种离散意图。

## 6. 为什么 score 段禁用 NodeResourcesFit 与 BalancedAllocation，但保留它们的 Filter

`deploy/base/configmap.yaml` 里只动了 `score` 段，`filter` 段保持默认插件集：

```yaml
plugins:
  score:
    enabled: [{name: tahrs, weight: 5}]
    disabled: [{name: NodeResourcesFit}, {name: NodeResourcesBalancedAllocation}]
```

`KubeSchedulerConfiguration` 的 `plugins` 是**增量**声明：没有出现的扩展点用默认插件集。`filter` 段的默认集合里
本来就有 `NodeResourcesFit` 与 `TaintToleration`，所以不写它们反而更稳（不会因为漏写某个默认插件而丢掉约束）。

- **Filter 必须保留**：`NodeResourcesFit` 的 Filter 负责「pods 数量上限」「node 资源是否装得下」「volume 数量」这些硬约束；
  我们的 Score 只处理软偏好，不能替代硬过滤（而且我们故意不重复实现它，避免两份规则分叉）。
- **Score 必须禁用**：否则同一个「资源比例」会被两个插件重复计分，`weight: 5` 的实际含义会随框架版本漂移；
  另外 `BalancedAllocation` 的方差式平衡与我们的 fit 目标相近但不相同，两个一起开会得到一个无法解释的合成分数。

## 7. 双调度器并发时的重复承诺（E7）

集群里默认调度器与 tahrs 同时存在，两者各自维护独立的 `Snapshot`，互相看不到对方刚做的 `assume`。
极端情况下：两个调度器在同一个快照窗口内分别把一个 Pod 绑到同一个 GPU 节点，造成真实的超卖。
这不是 tahrs 独有的问题（多调度器方案的通病），单靠插件无法解决——需要的是**减少两个调度器同时看到同一批节点**。

采用的方案是节点池隔离（`deploy/README.md`「节点池隔离约定」）：

- GPU 池节点打 label `scheduling.example.com/pool=heterogeneous` 与污点 `scheduling.example.com/dedicated=tahrs:NoSchedule`；
- 只有显式容忍该污点并声明 affinity 的工作负载才会进入 GPU 池，其余 Pod 由默认调度器处理，且默认调度器不会把它们放到 GPU 池；
- tahrs 只负责 GPU 池内的排序（它的画像本来就围绕 GPU 展开）。

这消除了绝大部分交叉窗口，但不能消除「两个 tahrs 副本」的竞争——所以 Deployment 固定单副本，并靠 Lease
（`tahrs-system/tahrs-scheduler`）保证同一时刻只有一个活跃调度器。

## 8. 为什么 `percentageOfNodesToScore: 100`

上游默认按百分比采样节点（默认 50%，小集群退化为 100）。采样对「找一个能放下的节点」够用，但对「在少数几个 GPU 节点里
选最合适的」是致命的：

- GPU 节点本来就少，采样可能只看到其中一两个，排序结果随机化；
- 采样比例随集群规模变化，导致同一组 Pod 在不同规模集群上行为不一致，回放工具也就无法解释线上结果。

本项目的目标集群规模是几十个节点，全量打分的成本可以接受（`test/framework` 的 16 节点并发测试在 `-race` 下 1 秒量级）。
所以显式写 100，配合 `deploy/base/configmap.yaml` 里的固定值，避免依赖默认值。

## 9. 为什么 v0.1 不实现 SignPlugin / OpportunisticBatching（E13）

OpportunisticBatching 允许框架把一个 Pod 的调度结果复用到同批次的「相同形状」的 Pod 上，跳过重复打分。
它依赖 `framework.SignPlugin` 给出签名，而签名一旦不完整，就会把不同画像、不同请求的 Pod 当成同一个，产生错误放置。

v0.1 不做，原因：

- 我们的输入里**画像名**来自注记，它不进 Pod spec，上游 batching 的签名自然会漏掉它；
- GPU 维度的值来自 `max(requests, limits)` 的适配逻辑，签名必须包含「有效请求」而不是原始 spec；
- 资源名 `gpuResourceName` 可配置，签名也要覆盖。

也就是说，要正确实现必须自己接管 `SignPlugin` 并让签名包含（画像名，有效请求向量，GPU 资源名）。
收益是打分次数减少，代价是一次错误的复用就是一次错误放置，v0.1 选择先不做。副作用是 batching 被关闭，
吞吐略低——我们在文档里记录这个取舍，而不是假装没有。

## 10. 为什么 go.mod 里有 33 条 replace（F15）

kube-scheduler 的 Framework 接口、`NodeInfo` 等类型住在 staging 模块（`k8s.io/kube-scheduler`、`k8s.io/api` 等），
而运行框架代码（`pkg/scheduler/framework/runtime`、`testing` 里的测试脚手架）住在 `k8s.io/kubernetes` 主模块。
主模块的 `go.mod` 用 `replace` 把每个 staging 模块指向仓库内的 `./staging/...` 目录，它发布时并不带这些目录。

下游模块一旦 import `k8s.io/kubernetes`，就必须把同样的 replace 抄一遍，否则 staging 模块会解析到各自发布的版本，
与主模块里预期的不一致（类型不兼容、接口漂移）。实测需要的条数是 **33 条**（与本仓库 `go.mod` 里
`// BEGIN k8s-staging-replace` 之间的行数一致），由 `hack/sync-replace.sh` 从上游 `go.mod` 自动生成：

```bash
make replace-sync     # 依赖 /home/qiai/go/pkg/mod 里的 k8s.io/kubernetes@v0.36.5 源码
```

代价是二进制很大：`go build ./cmd/tahrs-scheduler` 产物 **103,630,774 字节**（约 103 MB），
所以镜像用 `distroless/static:nonroot` 只取代运行库。这也是为什么仓库不 vendored 依赖：体积会翻倍。

## 11. 已知局限

- **单可行节点不调用 Score（F13）**：意味着在单节点集群里，评分公式完全不会被验证，而画像校验在 PreFilter 仍生效。
  评测用的更多节点场景走 `test/framework` 与 `tahrsctl replay`。
- **不支持 DRA（E4）**：`ResourceClaim` 的分配不进入账本。v1.36 默认启用 DRA，`ClusterRole` 里保留了相关规则，
  但账本口径没有接 `UseDRANodeAllocatableResourceClaimStatus` 之类的选项。
- **伪 GPU 不等价于真实分配**：测试用 `nvidia.com/gpu` 这个扩展资源名模拟，真实集群里 MIG、时间片共享、`nvidia.com/mig-*`
  都不是这个模型能表达的。
- **没有跑 KWOK 观察（T5 未做）**：`kwokctl` 可以离线伪造大量节点做分布对比，本机时间没花在这一步，
  所以文档里没有任何「装箱率提升 X%」的数字。§14 的措辞红线也是同一个原因：没有实测的提升不写。
- **镜像体积**：解压后 106.35 MB（二进制 103.27 MB + 基础层 2.61 MB），压缩层合计 48.57 MB；
  用仓库 Dockerfile 的 `-s -w` 构建时二进制为 71.5 MB（镜像未实测）。
- **集群内单副本冒烟未完成**：k3s 的 containerd socket 只有 root 可读，导入镜像这一步需要人工 sudo，
  命令写在 `deploy/README.md` 的「待办」一节；链路验证已用宿主机进程完成。

## 附：本仓库的可复现性约定

- 评价一个节点的顺序永远是 `Validate → Fit → Pack → clamp01 → round(100×) → 减罚分 → floor(0)`，
  `internal/scoring/evaluate.go` 的 `scoreFromComponents` 固化这个次序，`TestScoreCompositionOrder` 守着它；
- 维度顺序固定为 `cpu, memory, gpu`（`internal/model/resources.go` 的 `Dimensions` 数组），不 range map，避免浮点求和顺序漂移；
- 画像名排序固定（`config.ProfileNames`），指纹输出稳定，`internal/config/testdata/fingerprint.golden` 是 golden；
- 回放排序：分数降序，同分按节点名升序（`internal/replay/replay.go`），保证输出可 diff；
- 37 组评分 golden（`internal/scoring/testdata/golden_evaluate.json`，`go test ./internal/scoring -update` 重建）。
