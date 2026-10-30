# DRA 现场验证清单（未执行）

这目录里的东西只准备、不执行：**没有任何一条命令在当前集群上跑过**。目的是把「装上 DRA 之后 tahrs 会发生什么」变成一次可复现的现场实验，尤其是我们目前只能靠单测断言的两件事——DRA 分配的 GPU 不进 `NodeSnapshot` 账本、以及扩展资源桥的分数不变性。

## 1. 目标（三条，按价值排序）

| # | 要验证的 | 现在只能怎么验 |
|---|---|---|
| 1 | 纯 DRA 分配下 `NodeSnapshot.Used.GPUs == 0`，`tahrs_gpu_ledger_blind_total{reason="device_claims"}` 增长 | 单测（`internal/kubeadapter/draledger_test.go`、`internal/plugin/tahrs/dra_test.go`） |
| 2 | 桥（`DeviceClass.spec.extendedResourceName`）开启时，请求 `nvidia.com/gpu` 的 Pod 分数与纯扩展资源模式一致 | 文档里的推理，没有实测 |
| 3 | 真实 `ResourceSlice` 的容量约束（`capacity: memory >= 8Gi`）长什么样、选中哪台设备 | 无（假节点没有驱动，永远没有 ResourceSlice） |

## 2. 集群现状（2026-10-30 只读核对）

- k3s `v1.36.5+k3s1`，单节点 `llmops-wsl`。
- GPU：`NVIDIA GeForce RTX 5070 Ti Laptop GPU`，12227 MiB（12 GB），驱动 591.86，MIG `[N/A]`。
- `kube-system/nvidia-device-plugin-daemonset`（`nvcr.io/nvidia/k8s-device-plugin:v0.20.1`）在跑，节点 allocatable `nvidia.com/gpu: 1`。
- CDI 已生成：`/etc/cdi/nvidia.yaml`、`/var/run/cdi/nvidia.yaml`（`nvidia-ctk`、`nvidia-container-runtime` 都在）。
- 节点**没有**任何 NFD/GFD 标签，也没有 `nvidia.com/gpu.present`。
- DRA API 是 v1（`resourceslices`/`deviceclasses`/`resourceclaims`），但集群里**没有任何 ResourceSlice**——没有 DRA 驱动。

## 3. 前置条件与已知风险（按踩坑概率排序）

1. **节点标签**：上游 chart 的 kubelet-plugin 用 NFD 标签（`feature.node.kubernetes.io/pci-10de.present` 等）或 `nvidia.com/gpu.present=true` 选择节点。我们一个都没有，不打标签的话 DaemonSet 会一直 Pending。
   ```
   kubectl label node llmops-wsl nvidia.com/gpu.present=true
   ```
   这是集群改动（会改节点对象），执行前需要你点头。

2. **WSL 的驱动路径（最大的未知）**：chart 默认 `nvidiaDriverRoot=/`，把主机根挂进容器当 `/driver-root`，驱动靠 `NVIDIA_DRIVER_ROOT` 找 `libnvidia-ml.so.1`。这台机器的 NVML 在 `/usr/lib/wsl/lib/`，不在标准 ldconfig 路径里。可能的结果是 kubelet-plugin 启动时报 NVML 初始化失败。真出现了，再按日志决定是给主机建符号链接、还是 `--set` 挂载；**不要预先改主机**。

3. **与现有 device plugin 冲突**：`DeviceClass gpu.nvidia.com` 在 `resource.k8s.io/v1` 上默认带 `extendedResourceName: nvidia.com/gpu`（chart 里写死的），也就是装上即启用桥。而现有 device plugin 同样在公布 `nvidia.com/gpu`。两套系统同时认领同一个扩展资源，账本会互相打脸。做桥的实验前必须先停掉 device plugin：
   ```
   kubectl -n kube-system scale ds nvidia-device-plugin-daemonset --replicas=0
   ```
   恢复时 scale 回 1。

4. **ComputeDomains**：需要 MNNVL 硬件，单机没有，安装时关掉（`--set resources.computeDomains.enabled=false`），这样 kubelet-plugin 只有 1 个容器。

6. **capacity 请求**：`capacity.requests` 依赖 `DRAConsumableCapacity`（1.33 alpha → 1.34 beta，1.36 默认开启）。如果集群关掉了这个门控，claim 里的 `capacity` 字段会被拒。

5. GeForce 不支持 MIG，所以 `mig.nvidia.com` 那个 DeviceClass 装了也用不了；忽略即可。

## 4. 安装（需要 helm；当前机器未安装）

```bash
helm install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version 0.5.1 \
    --create-namespace \
    --namespace dra-driver-nvidia-gpu \
    --set gpuResourcesEnabledOverride=true \
    --set resources.computeDomains.enabled=false
```

镜像：`registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v0.5.1`。

这里**不 vendor 渲染后的静态清单**：chart 有 controller、kubelet-plugin、RBAC、可选 webhook/VAP 二十多个对象，手工抄一份很快就和上游漂移。要么用上面的 helm，要么在离线环境用 `helm template` 生成后单独存放（不放进本仓库）。

## 5. 验证步骤（全部只读或可回滚）

| 步 | 命令 | 预期 |
|---|---|---|
| 1 | `kubectl -n dra-driver-nvidia-gpu get pods` | `kubelet-plugin` 1/1 Running |
| 2 | `kubectl get deviceclass` | 出现 `gpu.nvidia.com`；`kubectl get deviceclass gpu.nvidia.com -o jsonpath='{.spec.extendedResourceName}'` 应为 `nvidia.com/gpu` |
| 3 | `kubectl get resourceslices -o wide` | 每个 GPU 节点一条 `gpu.nvidia.com` 的 slice，devices 里 `capacity.memory` 约 `12227Mi` |
| 4 | `kubectl -n dra-probe apply -f example-claim.yaml` | claim 分配成功，Pod Running，`kubectl exec` 里 `nvidia-smi -L` 能看到卡 |
| 5 | 停掉 device plugin 后看 tahrs | 节点 allocatable `nvidia.com/gpu` 变 `0`；调度器日志出现我们新增的「账本看不见 GPU」告警；`tahrs_gpu_ledger_blind_total{reason="device_claims"}` 从 0 变 1 |
| 6 | `kubectl -n dra-probe apply -f example-bridge-pod.yaml` | 请求 `nvidia.com/gpu: 1` 的 Pod 被自动创建 claim 并调度成功；分数应与「有 device plugin 但没有 DRA」时一致 |

第 5 步是整个实验的核心：**纯 DRA 的 Pod（只挂 claim、不写 `nvidia.com/gpu`）在 tahrs 眼里就是 0 卡任务**，GPU 维度对它完全失效；而节点上真实被占用的那 12 GB 显存，`NodeSnapshot.Used.GPUs` 永远是 0。

## 6. 回滚

```bash
kubectl delete -k deploy/dra            # 删示例 Pod / claim
helm uninstall dra-driver-nvidia-gpu -n dra-driver-nvidia-gpu
kubectl -n kube-system scale ds nvidia-device-plugin-daemonset --replicas=1
kubectl label node llmops-wsl nvidia.com/gpu.present-
```

## 7. 文件清单

| 文件 | 作用 |
|---|---|
| `kustomization.yaml` | 示例负载的入口（命名空间 + 两个 Pod 清单） |
| `example-claim.yaml` | 纯 DRA 路径：`memory >= 8Gi` 的容量约束 + 一个不请求 `nvidia.com/gpu` 的 Pod |
| `example-bridge-pod.yaml` | 桥路径：`requests: nvidia.com/gpu: 1` 的普通 Pod |
| `verify.sh` | 只读自检：DeviceClass、ResourceSlice、claim、节点账本，一条命令打印全部观察点 |

## 8. 明确没做的事

- 没装 helm、没装驱动、没打节点标签、没停 device plugin；
- 没有 `ResourceSlice`、没有 claim 被创建；
- 以上全部是「文档 + 清单」，命令一条未执行。
