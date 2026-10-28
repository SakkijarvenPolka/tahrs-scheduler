# tahrs-scheduler 部署清单

计划 §7 的交付物：只交付 manifest、Dockerfile 与 Makefile，不做压测演练。

## 目录

| 文件 | 内容 |
|---|---|
| `base/namespace.yaml` | `tahrs-system` 命名空间 |
| `base/serviceaccount.yaml` | 调度器专用 SA（不绑 `system:kube-scheduler`） |
| `base/clusterrole.yaml` | **生成文件**，见下一节 |
| `base/clusterrolebinding.yaml` | 把 `tahrs-scheduler-role` 绑到该 SA |
| `base/role-kube-system.yaml` | 读 `kube-system/extension-apiserver-authentication` 的 Role/RoleBinding |
| `base/role-lease.yaml` | Lease 权限，create 与 get/update 拆成两条 |
| `base/configmap.yaml` | `kube-scheduler-config.yaml` + `tahrs-config.yaml` |
| `base/deployment.yaml` | 单副本 Deployment，配置只读挂载 |
| `base/kustomization.yaml` | `kubectl apply -k deploy/base` |
| `common/examples/gpu-demo.yaml` | GPU 工作负载接入示例（§9） |

## RBAC 的来源

`base/clusterrole.yaml` 由 `hack/gen-rbac` 从上游
`k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go`
的 `system:kube-scheduler`（policy.go:670）与 `system:volume-scheduler`（policy.go:528）
合并生成，重新生成用：

```bash
make rbac
```

对上游角色做了三处刻意偏离，生成器头部注释里也写了：

1. **Lease 权限不进 ClusterRole**：上游把 leases 规则放在 `system:kube-scheduler` 里
   （policy.go 上还挂着 `TODO: scope this to the kube-system namespace`），经
   ClusterRoleBinding 之后等于每个命名空间都能建 Lease。本仓库的 leader election 只碰
   `tahrs-system/tahrs-scheduler`，所以生成器把这条剔除，改由 `role-lease.yaml` 授予：
   `create` 不加 `resourceNames`（首次竞选要能创建），`get/update` 限定到这一条 Lease。
2. **角色改名** `tahrs-scheduler-role`，避免和集群里已有的默认角色重名。
3. **`extension-apiserver-authentication` 读取放在 kube-system 的 Role 里**，不进 ClusterRole：
   ClusterRoleBinding 会让 ConfigMap 读权限覆盖所有命名空间，比 api-server 的接口约定宽。
   动词是 `get/list/watch`：上游用 filtered informer 监听这个 ConfigMap
   （`apiserver/pkg/server/dynamiccertificates/configmap_cafile_content.go`），只给 `get`
   会让 informer 永远同步不上，客户端证书认证被静默关掉；RBAC 无法用 `resourceNames`
   限制 `list/watch`，所以它在 kube-system 内是命名空间级权限。

`resource.k8s.io` 的 resourceslices/deviceclasses/resourceclaims、`storage.k8s.io` 的
csistoragecapacities/volumeattachments、`namespaces` 都来自上游规则（DRA 在 v1.36 默认开启，
所以生成结果里包含这些规则，共 27 条）。

## 镜像

```bash
make image            # docker build，镜像名 tahrs-scheduler:v0.1.0
```

Dockerfile 两段：`golang:1.27` 构建（`CGO_ENABLED=0`、`-trimpath`、`-s -w`），
运行时用 `gcr.io/distroless/static:nonroot`，以 uid 65532 运行。

本机 WSL 没有运行中的 Docker：`docker` 命令来自 Docker Desktop 但引擎未启动，
因此上面的镜像由 `ko` 等价构建（同样使用 distroless static 基础镜像、同样非 root）：

```bash
KO_DOCKER_REPO=tahrs-scheduler ko build --bare --platform=linux/amd64 \
  --push=false --tarball=/tmp/tahrs-scheduler.tar --tags=v0.1.0 ./cmd/tahrs-scheduler
```

测得体积：**未压缩 106.35 MB**（其中二进制 103.27 MB + distroless 基础层 2.6 MB），
**压缩后 48.57 MB**（`docker save` 出来的 tar 48.57 MB）。
注意 ko 默认不加 `-s -w`，所以二进制偏大；用仓库 Dockerfile 构建的二进制实测 **71.5 MB**，
预计镜像约 **74 MB**（未实测，等有 Docker 引擎时补记）。

导出的镜像归档需要导入到 k3s 的 containerd 才能在集群里用：

```bash
sudo k3s ctr -n k8s.io images import /tmp/tahrs-scheduler.tar   # 需要 root
```

## 应用

```bash
kubectl create namespace tahrs-system        # 或先单独 apply base/namespace.yaml
kubectl apply -k deploy/base
```

`kubectl apply --dry-run=server -k deploy/base` 在本机 k3s（v1.36.5+k3s1）上通过（10 个资源）。
注意 server dry-run 不会顺带创建命名空间，所以命名空间要先存在，否则命名空间级资源会报
`namespaces "tahrs-system" not found`。

## 单副本与健康信号

- `replicas: 1`，Lease `tahrs-system/tahrs-scheduler`。
- 配了 `startupProbe`/`livenessProbe`/`readinessProbe`，分别打 secure 端口的
  `/healthz`、`/livez`、`/readyz`。这三个路径由默认的 `system:public-info-viewer`
  ClusterRole 对未认证用户放行，kubelet 的无凭据 HTTPS GET 能通过，形状与 kubeadm
  静态 Pod 一致；distroless 镜像里没有 shell 和 curl，exec 探针不可用。
  **这三个探针没有在集群内实测过**（导入镜像需要 root）：部署后先确认 Pod 不变
  CrashLoopBackOff。真正的健康信号仍是副本数、Lease 持有者和启动打印的配置指纹。
- 调度器 Pod 自身由集群默认调度器调度（没有设置 `schedulerName`），避免自调度死锁。
- 配置改动需要 `kubectl rollout restart deployment/tahrs-scheduler`，没有热加载（§4.4）。

## 节点池隔离约定（写进 docs）

pool label 打在**所有由 tahrs 管理的节点**上（一个异构池，可以同时含 CPU 节点和 GPU 节点）：

- label `scheduling.example.com/pool=heterogeneous`（池内全部节点）
- 污点 `scheduling.example.com/dedicated=tahrs:NoSchedule`（只有 GPU 节点）

隔离由两侧各自强制，不依赖工作负载作者自觉：

- 调度器侧：`configmap.yaml` 里的 `NodeAffinity` 插件配置了 `addedAffinity`，要求
  `pool=heterogeneous`。工作负载即使忘了写 nodeAffinity，tahrs 也不会把它放到池外节点。
- 集群侧：GPU 节点的污点让 default scheduler 不会把普通 Pod 放上去；工作负载要落到 GPU 池
  仍需自己声明容忍与 nodeAffinity（见 `common/examples/gpu-demo.yaml`）。

这条约定解决 §7 的双调度器重复承诺问题。

## 本机验证记录（2026-10-28）

| 项 | 结果 |
|---|---|
| `kubectl apply --dry-run=server -k deploy/base` | 通过，10 个资源 |
| 镜像导入 k3s + 单副本部署 | **未执行**：k3s 的 containerd socket 只有 root 可读，当前环境 sudo 需要密码。留作待办，命令见本节末尾 |
| 链路验证（宿主机进程 + k3s API） | Pod `tahrs-chain-test` 由 `tahrs-scheduler` 绑定到 `llmops-wsl`，事件 `reportingComponent=tahrs-scheduler`，日志 `Successfully bound pod to node evaluatedNodes=1 feasibleNodes=1` |
| F13 现象 | 该次调度 `Scored node` 计数为 0——单可行节点时框架跳过 Score，画像校验在 PreFilter 已生效，与设计一致 |

## 待办：集群内单副本冒烟

k3s 的 `/run/k3s/containerd/containerd.sock` 权限是 `root:root 0660`、`/run/k3s` 目录 `0750`，
`sudo` 需要密码，所以镜像导入这一步没有在本机执行。拿到 root 后按下面四步补：

1. 导入镜像（镜像我已用 ko 构建：`/tmp/tahrs-scheduler.tar`，48.6 MB）：

   ```bash
   sudo k3s ctr -n k8s.io images import /tmp/tahrs-scheduler.tar
   ```

2. 部署并等待就绪：

   ```bash
   kubectl create namespace tahrs-system
   kubectl apply -k deploy/base
   kubectl -n tahrs-system get pods -w          # 等 1/1 Running
   ```

3. 核对启动指纹与 Lease 持有者：

   ```bash
   kubectl -n tahrs-system logs deploy/tahrs-scheduler | grep "Loaded tahrs"
   kubectl -n tahrs-system get lease tahrs-scheduler -o jsonpath='{.spec.holderIdentity}'
   ```

4. 清理：

   ```bash
   kubectl delete -k deploy/base && kubectl delete namespace tahrs-system
   ```

完整验收命令（root 已就绪时）：

```bash
sudo k3s ctr -n k8s.io images import /tmp/tahrs-scheduler.tar
kubectl apply -k deploy/base
kubectl -n tahrs-system get pods -w          # 等 1/1 Running
kubectl -n tahrs-system logs deploy/tahrs-scheduler | grep "Loaded tahrs"
kubectl -n tahrs-system get lease tahrs-scheduler -o jsonpath='{.spec.holderIdentity}'
```
