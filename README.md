# tahrs-scheduler

一个面向异构节点的 Kubernetes 放置调度器插件：同时评估「装得下」（fit）与「装得紧凑」（pack），
把不同业务的放置偏好写进配置里的画像（profile），Pod 用一行注解选择画像；每一次决策都能离线用
`tahrsctl replay` 逐节点解释，不需要集群。

## 它解决什么问题

默认调度器的打分只有「越空越好」：`LeastAllocated` 把负载摊平，既无法表达「这类负载应该装箱」，
也无法表达「普通 Pod 不该占用 GPU 池」。tahrs 只做打分这一层（硬约束仍交给 `NodeResourcesFit`、
`TaintToleration` 等原生插件），把放置偏好收敛成三件事：

- **形状匹配（fit）**：节点剩余资源与 Pod 请求越接近，分数越高；
- **紧凑度（pack）**：Pod 放上去之后节点越满，分数越高，方向可整项反转；
- **GPU 偏好**：画像声明 `allow` / `avoid`，非 GPU 负载在 GPU 节点上按固定罚分扣分。

## 快速开始

### 构建

```bash
make build     # 产出 bin/tahrs-scheduler 与 bin/tahrsctl
make test      # 全量单测（带 -race）
```

### 离线解释一次决策

`docs/examples/` 里有一份最小输入（配置、工作负载、节点快照），不需要集群：

```bash
./bin/tahrsctl replay \
  --config docs/examples/tahrs-config.yaml \
  --pod    docs/examples/workload.yaml \
  --nodes  docs/examples/cluster.yaml
```

```
profile=gpu-inference-v1 configHash=c1a805d7
NODE        FIT     PACK    PENALTY  SCORE  NOTE
gpu-node-1  0.4194  0.5581  0        52
gpu-node-2  0.3023  0.4697  0        42
cpu-node-1  0.0000  0.0000  0        0      error: node allocatable is zero for a requested dimension: node "cpu-node-1" dimension "gpu" requested=1 allocatable=0
```

每行是一个候选节点：`FIT`/`PACK` 是分量，`PENALTY` 是 GPU 罚分，`SCORE` 是最终分（0–100）。
无法计算的节点会给出 NOTE，而不是静默记 0 分。

### 在集群里部署

```bash
kubectl apply -k deploy/base                            # Namespace、SA、RBAC、ConfigMap、Deployment
kubectl -n tahrs-system logs deploy/tahrs-scheduler     # 启动日志含 configVersion 与配置指纹
```

提交一个交给 tahrs 的 Pod：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: llama-7b-0
  annotations:
    scheduling.example.com/profile: gpu-inference-v1
spec:
  schedulerName: tahrs-scheduler
  containers:
  - name: app
    image: your-image
    resources:
      requests: {cpu: "2", memory: 8Gi, nvidia.com/gpu: "1"}
      limits:   {cpu: "2", memory: 8Gi, nvidia.com/gpu: "1"}
```

`schedulerName` 必须写。没有这一行的 Pod 仍由默认调度器处理——部署出来的 tahrs 是第二调度器，
不会接管现有工作负载。未通过画像或资源校验的 Pod 会留在 Pending，原因在事件里：

```bash
kubectl describe pod llama-7b-0 | sed -n '/Events/,$p'
kubectl get events --sort-by=.lastTimestamp | grep -i tahrs
```

### 本地伪节点试跑（不需要真集群）

```bash
make local-test                                  # KWOK 造 4 个伪节点，跑三条落点断言
./test/local/compare.sh                          # 同一夹具对比 tahrs 与原生调度器
NODEFILE=$PWD/test/local/nodes-200.yaml PODS=300 ./test/local/compare.sh   # 200 节点规模
```

KWOK 只模拟 kubelet：API Server、调度框架和 tahrs 二进制都是真的，容器不会真跑。脚本结束时
删除伪节点与测试命名空间，不会影响集群里已有的负载。

## 配置

```yaml
configVersion: "2026-10-tahrs1"
gpuResourceName: nvidia.com/gpu
defaultProfile: cpu-general-v1
gpuAvoidPenalty: 30
profiles:
  cpu-general-v1:
    alpha: 0.3
    beta: 0.7
    fitWeights:  {cpu: 1, memory: 1}
    packWeights: {cpu: 1, memory: 1}
    gpuNodePolicy: avoid
    packMode: pack
```

| 字段 | 含义 | 默认 |
| --- | --- | --- |
| `configVersion` | 配置版本号，会打到启动日志与指纹里 | 必填 |
| `gpuResourceName` | GPU 扩展资源名 | `nvidia.com/gpu` |
| `defaultProfile` | Pod 未写画像注解时使用的画像 | 必填 |
| `gpuAvoidPenalty` | `avoid` 画像下，非 GPU 负载落在 GPU 节点的扣分 | `30` |
| `profiles.<name>.alpha` / `beta` | fit 与 pack 的配比，两者之和不必为 1 | `0.3` / `0.7` |
| `fitWeights` / `packWeights` | 三个维度（`cpu`、`memory`、`gpu`）各自的权重 | 全 0 → 校验失败 |
| `gpuNodePolicy` | `allow` 或 `avoid` | `avoid` |
| `packMode` | `pack` 或 `spread`（pack 项取反） | `pack` |

配置在启动时一次性加载并校验，错误会列全而不是只报第一条；启动日志打印 `configVersion` 与配置
指纹（sha256 前 8 位）。修改配置需要重启，没有热加载。

## Pod 侧的三个入口

1. **画像**：`scheduling.example.com/profile` 注解；不写则用 `defaultProfile`，写了不存在的画像会被
   明确拒绝（而不是静默回落）。
2. **资源**：`requests` 决定请求向量；GPU 取 `max(requests, limits)`，与上游账本一致。
3. **硬约束**：`nodeSelector`、`nodeAffinity`、容忍、拓扑分布等原生字段照常生效。

画像_name_是任务与调度策略之间唯一的耦合点：任务只能选择画像，不能覆盖画像里的权重。

## 已实现的功能

**调度插件**

- PreFilter 解析画像并写入 CycleState；PreScore 校验状态，并识别「GPU 账本不可见」的形状。
- Score：`α·fit + β·pack`（clamp 到 [0,1]）放大到 100 分制，减去 GPU 罚分，下限 0；单个节点算不出来
  时回退 MinScore 并计数，不让整批 Pod 失败。
- `packMode: pack | spread`：pack 项方向可翻转，既用于打散型画像，也用于受控对照实验。
- EnqueueExtensions：只有画像注解发生变化的 Pod 才唤醒调度队列，避免无关事件引发全量重排。
- DRA 兼容：设备经 DRA 分配时扩展资源账本为 0，插件会计数并发首次告警；持有 device claims 的 Pod
  跳过 GPU 回避罚分，避免同一份约束被扣两次。
- 确定性：评分是纯函数，不读时钟、不遍历 map，同一输入给同一分数。

**指标**（`/metrics`，前缀 `tahrs_`）

`profile_resolution_total`、`score_duration_seconds`、`score_component`、`gpu_penalty_total`、
`invalid_numeric_total`、`gpu_ledger_blind_total`、`config_info`。

**工具与资产**

- `tahrsctl replay`：离线回放，输出如上表；与调度器共用同一份配置和评分库。
- `hack/gen-rbac`：从上游 `system:kube-scheduler` 与 `system:volume-scheduler` 策略生成 ClusterRole，
  Lease 权限单独拆成命名空间 Role。
- `deploy/base`：kustomize 清单（Namespace、SA、RBAC、ConfigMap、Deployment），distroless 镜像，
  启动/存活/就绪三个探针。
- `deploy/dra`：真卡 DRA 验证套件（驱动安装、DeviceClass、示例 claim），用于有 NVIDIA GPU 的环境。
- `test/local`：KWOK 伪节点端到端（`run.sh`）与三方对照（`compare.sh`），支持 200 节点夹具、
  串行到达、重复运行与确定性检查。
- 测试：评分 golden 用例、配置校验、账本适配、插件生命周期、框架级验收（镜像上游调度流程）、
  本地端到端。

## 文档

- [docs/architecture.md](docs/architecture.md)：设计与取舍——为什么只做打分、为什么禁用两个原生打分
  插件、F11/F12/F13 的实现、DRA 边界。
- [deploy/README.md](deploy/README.md)：部署清单、RBAC 三处偏离说明、镜像构建、节点池隔离约定。
- [deploy/dra/README.md](deploy/dra/README.md)：DRA 验证套件的安装、验证与回滚。
- [test/local/README.md](test/local/README.md)：本地伪节点测试、三方对照方法与实测结果。

## 边界

- 只做放置打分，不做硬过滤，也不替代 `NodeResourcesFit`；Filter 段保持默认插件集。
- GPU 按整卡计数，不读设备属性（型号、显存、MIG）。需要「一张 40G 卡」这类语义时应使用 DRA；
  插件对 DRA 分配只提供账本盲区可见性，不参与设备选择。
- 配置启动加载、无热更新；调度器本身无状态，单副本即可，多副本由 leader election 选主。
- 与 kube-scheduler v1.36 的框架 API 对齐；评分库不依赖 k8s，可独立复用与单测。
