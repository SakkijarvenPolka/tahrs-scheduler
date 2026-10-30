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