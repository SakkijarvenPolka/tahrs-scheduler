# 本地伪节点调度测试

`test/local/run.sh` 在一个真实集群上跑完整调度链路：**真实的 API server + 真实的
kube-scheduler 框架 + 真实的 `tahrs-scheduler` 二进制**，节点由
[KWOK](https://kwok.sigs.k8s.io) 伪造，只有 kubelet 是模拟的。

这条路径覆盖 `test/framework`（Go 单测，手工构造 NodeInfo，直接调插件）和
`tahrsctl replay`（离线打分，不做 Filter）都覆盖不到的东西：

- 真框架的 Filter → PreFilter → PreScore → Score → Bind 全流程；
- `NodeAffinity.addedAffinity` 真的作用在 Pod 上（`test/framework` 不加载框架的
  NodeAffinity 插件，只有这里能验证）；
- 污点、扩展资源 `nvidia.com/gpu` 的过滤与计分；
- Pod 被拒时用户实际看到的事件文案（PreFilter 的 `unknown profile`）。

## 依赖

- 一个可用的集群和 cluster-admin 权限（本机 k3s 单节点即可）；
- `kubectl`、`go`、`curl`、`python3`（需要 PyYAML：`pip install pyyaml`）；
- KWOK：PATH 与 `$(go env GOPATH)/bin` 里有 `kwok` 就直接用；没有的话首次运行会下载约 116 MB
  的二进制和两个清单到 `~/.cache/tahrs-local/`，之后离线可用。也可以自己
  `go install sigs.k8s.io/kwok/cmd/kwok@v0.8.0`，再用 `KWOK=... ./test/local/run.sh` 指定。

## 用法

```bash
./test/local/run.sh            # 跑完自动清理命名空间、伪节点和临时目录
KEEP=1 ./test/local/run.sh     # 保留命名空间、伪节点和日志，便于手工排查
make local-test                # 同上，走 Makefile 目标
```

可选环境变量：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `NAMESPACE` | `tahrs-local-test` | 测试工作负载所在命名空间 |
| `KWOK_VERSION` | `v0.8.0` | KWOK 版本 |
| `KWOK_PORT` | `10247` | KWOK 的模拟 kubelet 端口 |
| `SCHED_PORT` | `10279` | 调度器 secure 端口（只为了不撞 10259） |
| `KWOK` | 自动探测 | `kwok` 二进制路径 |
| `NODEFILE` | `test/local/nodes.yaml` | 伪节点清单；规模场景指向 `nodes-200.yaml` |
| `PODS` | `0` | 大于 0 时追加一批 `tahrs-batch-*` Pod，并统计落点与耗时 |
| `REAL_NODE` | `0` | 设为 `1` 时把真实节点临时打上 pool 标签，退出时还原 |
| `KEEP` | `0` | 设为 `1` 时保留现场 |
| `CACHE` | `~/.cache/tahrs-local` | KWOK 二进制与清单缓存目录 |

脚本用 `kubectl config view --raw --flatten` 生成自包含 kubeconfig，调度器以集群外
进程的方式连接；`deploy/base/configmap.yaml` 里的 `kube-scheduler-config.yaml` 与
`tahrs-config.yaml` 会被原样取出，只改 `clientConnection.kubeconfig` 和
`configPath`，所以测的就是仓库里要发布的那份配置。

## 伪节点

`nodes.yaml` 定义四个节点（`scheduling.example.com/pool=heterogeneous`）：

| 节点 | CPU / 内存 | GPU | 污点 |
| --- | --- | --- | --- |
| `tahrs-fake-cpu-1` | 32 / 128Gi | – | – |
| `tahrs-fake-cpu-2` | 8 / 32Gi | – | – |
| `tahrs-fake-gpu-1` | 32 / 128Gi | 4 | `dedicated=tahrs:NoSchedule` |
| `tahrs-fake-gpu-2` | 64 / 256Gi | 8 | `dedicated=tahrs:NoSchedule` |

`nodes-200.yaml` 是同一模板放大到 200 个节点（134 CPU + 66 GPU）的规模夹具，容量在三种
规格里循环，用来观察打分在节点数上去之后的行为：

```bash
NODEFILE=$PWD/test/local/nodes-200.yaml PODS=300 ./test/local/run.sh
```

集群里真正的那个节点（本机 k3s 的节点）**默认不打 pool 标签**，所以它也是
`addedAffinity` 的反例：任何 Pod 都不该落上去。加 `REAL_NODE=1` 时脚本会临时给它打上
`scheduling.example.com/pool=heterogeneous`（退出时删除），于是它和伪节点一样是合法落点——
本机因此可以混进池子。注意它上面是真 kubelet，落上去的 Pod 会真的去拉镜像。

## 断言

`workloads.yaml` 三个 Pod，全部 `schedulerName: tahrs-scheduler`：

| Pod | 期望 | 验证的机制 |
| --- | --- | --- |
| `tahrs-cpu` | 落到 `tahrs-fake-cpu-*` | 默认画像 + GPU 节点污点过滤 |
| `tahrs-gpu` | 落到 `tahrs-fake-gpu-*` | `gpu-inference-v1` 画像 + GPU 计分 + 容忍污点 |
| `tahrs-bad-profile` | 一直 Pending，事件含 `unknown profile` | PreFilter 拒绝未知画像 |

`PODS=300` 时还会追加 300 个 `tahrs-batch-*` Pod（1C/2Gi），统计「apply 到全部拿到
`nodeName`」的耗时、每 Pod 平均耗时和落点前 10 的节点。一次实测（200 节点、300 Pod、
k3s v1.36.5、本机）：4.7 s 全部调度完成，约 15 ms/Pod；落点集中在小 CPU 节点（8C/32Gi
每台 8 个），符合 fit/pack 同向的装箱行为。这是行为观察，不是性能基准。

## 三方对照：pack / spread / 原生

`compare.sh` 在完全相同的伪节点、Pod 模板和空集群上依次跑几轮，只有调度器（以及 spread 轮
里的一个画像字段）不同：

| 轮次 | 调度器 | 打分 |
| --- | --- | --- |
| `tahrs` | tahrs 二进制，发布配置 | `α·fit + β·pack` |
| `spread` | 同一个二进制、同一个画像名 | `α·fit + β·(1−pack)`，画像里只多一行 `packMode: spread` |
| `native` | 集群内置 kube-scheduler | 上游默认插件集（不是控制臂，只是外部参照） |

`spread` 轮是**受控对照**：除 pack 项的方向外，fit 权重、α/β、GPU 罚分、画像解析全部不变，
两轮的差异只能来自这一项。它不等于上游 `LeastAllocated`——pack 用的是「放上去之后」的使用率
`u'`，取反后等价于按 RMS 使用率排序，大节点天然更占便宜，所以「spread」轮会先填大节点而不是
把负载摊平。这本身就是结论的一部分，不要把它当成「打散」的等价物。

```bash
./test/local/compare.sh                                            # 三轮，300 Pod，允许进 GPU 节点
ARMS="tahrs spread" ./test/local/compare.sh                        # 只跑受控的两轮
ARMS="tahrs spread native" SERIAL=1 RUNS=2 ./test/local/compare.sh # 串行到达 + 重复两遍，查确定性
TOLERATE_GPU=0 ./test/local/compare.sh                             # 都不许进 GPU 节点
PODS=600 ./test/local/compare.sh                                   # 超过单个小节点类别的容量
```

指标：落点分布（用到的节点、每节点均值/标准差/最大、按节点类别拆分）、GPU 节点落点、
CPU 利用率变异系数、绑定吞吐、**碎片度**（跑完后还能放下一个 4C/8Gi 或 8C/16Gi Pod 的
节点数）、采样到的峰值 RSS（tahrs 两轮含调度器进程，native 轮只有 kwok——内置调度器跑在
k3s-server 进程里，不在采样范围）。

`SERIAL=1` 逐个创建 Pod 并等前一个拿到 `nodeName` 再建下一个，输入顺序确定；`RUNS=2` 会把
每一轮重跑一遍，并在报告末尾逐 Pod 比较两遍的落点是否一致，这是确定性检查。

本次实测（200 节点、300/600 Pod、k3s v1.36.5、单机 WSL2、2026-10-30）：

第 1 轮，300 Pod，允许进入 GPU 节点（Pod 带污点容忍）：

| 指标 | tahrs | spread | native |
| --- | --- | --- | --- |
| 绑定 300 Pod（ms） | 4246 | 4674 | 4541 |
| 用到的节点 | 27 | 133 | 151 |
| 每节点 Pod（均值 / 最大） | 11.1 / 16 | 2.3 / 4 | 2.0 / 5 |
| 落在 GPU 节点 | 0 | 0 | 169 |
| CPU 利用率 CV | 2.56 | 0.72 | 0.59 |
| 仍能放下 4C/8Gi 的节点 | 174 | 200 | 200 |
| 仍能放下 8C/16Gi 的节点 | 174 | 156 | 200 |
| 采样峰值 RSS（MiB） | 161 | 163 | 73 |

第 2 轮，300 Pod，普通 Pod 不许进 GPU 节点（`TOLERATE_GPU=0`）：

| 指标 | tahrs | spread | native |
| --- | --- | --- | --- |
| 绑定 300 Pod（ms） | 4295 | 4604 | 4466 |
| 用到的节点 | 21 | 134 | 89 |
| 每节点 Pod（均值 / 最大） | 14.3 / 32 | 2.2 / 4 | 3.4 / 5 |
| CPU 利用率 CV | 2.93 | 0.71 | 1.15 |
| 仍能放下 4C/8Gi 的节点 | 180 | 200 | 200 |

第 3 轮，600 Pod（超过单个小节点类别的容量）：

| 指标 | tahrs | spread | native |
| --- | --- | --- | --- |
| 绑定 600 Pod（ms） | 10941 | 10958 | 10492 |
| 用到的节点 | 35 | 134 | 155 |
| 每节点 Pod（均值 / 最大） | 17.1 / 32 | 4.5 / 8 | 3.9 / 10 |
| 落在 GPU 节点 | 0 | 0 | 344 |
| 仍能放下 4C/8Gi 的节点 | 165 | 200 | 200 |

第 4 轮，串行到达、重复两遍（60 Pod）：

| 指标 | tahrs | spread |
| --- | --- | --- |
| 用到的节点 | 8 | 60 |
| 每节点 Pod（均值） | 7.5 | 1.0 |
| 两遍落点完全相同的 Pod | 0/60 | 1/60 |

结论（只对本夹具成立）：

1. **落点差异全部来自打分方向**。tahrs 的 fit 与 pack 同向，300 Pod 只用了 27 个节点，单节点最多
   16 个；原生 `LeastAllocated` 用了 151 个节点，单节点最多 5 个。600 Pod 时 tahrs 仍是 35 个节点、
   单节点 32 个（16C 节点被填满），原生铺到 155 个节点。
2. **只有 GPU 偏好项是原生没有对应语义的**：允许进 GPU 节点时，原生把 169 个 Pod 放进 GPU 节点
   （600 Pod 时 344 个），tahrs 是 0。差别不在于 taint——两边的 Pod 都容忍了污点，tahrs 是在打分
   阶段用 `gpuAvoidPenalty` 把它压下去。
3. **spread 臂不是「摊平」**：它按放上去之后的 RMS 使用率排序，300 Pod 用了 133 个节点，但 32C
   节点承载 169 个、8C 节点 44 个，等于按容量比例分配，而不是每台一个。要复现上游「每台一个」的
   落点得用 `1 − Σ(used/A)/N`，那是另一个公式，不在这个开关的范围内。
4. **吞吐没有差异**（第 1 轮 64–71 Pod/s，600 Pod 时 55–57 Pod/s）：200 节点规模下打分不是瓶颈。
   峰值 RSS 里 tahrs 两轮是 161–183 MiB（含 kwok 约 59 MiB 的基线），native 轮只采到 73 MiB，
   因为内置调度器跑在 k3s-server 进程里，没有被单独采样，这两个数不可直接相减。
5. **逐 Pod 落点不可复现**（0/60、1/60）：同分节点由框架随机挑选，这不是插件的不确定性——插件对
   同一输入给的分是确定的，`internal/scoring` 的单测覆盖了这一点。要验证插件侧确定性，别看这一项。

这些都是行为观察，不是性能基准：伪节点没有真实 kubelet、没有设备插件，GPU 是假的。

## 加一个场景

改 `nodes.yaml` 加节点（或复制一份新夹具、用 `NODEFILE` 指向它）、改 `workloads.yaml`
加 Pod，然后在 `run.sh` 的断言段加一条 `case "$node" in ...` 即可。KWOK 节点除了 `capacity/allocatable` 也能带
`status.conditions`、`spec.taints`、任意 label，适合构造「同构但容量不同」或
「GPU 型号不同」的对比。

## 局限

- 伪节点的资源账本是调度器自己算的（绑定后 NodeInfo 累积），没有真实 kubelet
  上报，所以不会出现真实的资源超卖或 eviction；
- KWOK 不会真的拉镜像、跑容器，Pod 状态由 stage 直接改写；
- 这些数字只能证明「调度顺序/落点符合画像配置」，不能当作性能或装箱率收益。