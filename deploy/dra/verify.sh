#!/usr/bin/env bash
# 只读自检：把 DRA 验证需要的观察点一次打印出来。
# 不 apply、不删除、不改任何对象。
#
#   ./deploy/dra/verify.sh
#
# 每条输出后面带一行 expected，写明装上驱动之后应该看到什么。
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NODE="${NODE:-llmops-wsl}"
NS="${NS:-dra-probe}"

say() { printf '\n== %s\n' "$1"; }
want() { printf '   expected: %s\n' "$1"; }
show() { if [ -z "$1" ]; then echo "   (empty)"; else echo "   $1"; fi; }

say "节点 GPU 标签（缺了 kubelet-plugin 不会被调度）"
show "$("$KUBECTL" get node "$NODE" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.present}' 2>/dev/null)"
want "true；没有就先 kubectl label node $NODE nvidia.com/gpu.present=true"

say "节点 allocatable（device plugin 停掉后应变 0）"
show "$("$KUBECTL" get node "$NODE" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null)"
want "有 device plugin 时是 1；停掉之后应为空 —— 这就是 tahrs 账本看不见 GPU 的窗口"

say "DeviceClass"
show "$("$KUBECTL" get deviceclass gpu.nvidia.com -o jsonpath='{.spec.extendedResourceName}' 2>/dev/null)"
want "nvidia.com/gpu（桥默认开启）；对象不存在说明驱动还没装"

say "ResourceSlice 概况"
"$KUBECTL" get resourceslices -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,DRIVER:.spec.driver' 2>/dev/null || echo "   (none)"
want "每个 GPU 节点至少一条 driver=gpu.nvidia.com 的 slice"

say "每张卡的显存容量"
"$KUBECTL" get resourceslices -o jsonpath='{range .items[*]}{.spec.nodeName}: {range .spec.devices[*]}{.name}={.capacity.memory.value} {end}{"\n"}{end}' 2>/dev/null || true
want "12GB 卡约 12227Mi；没有 slice 说明 kubelet-plugin 没起来或没连上 apiserver"

say "kubelet-plugin 状态"
"$KUBECTL" -n dra-driver-nvidia-gpu get pods 2>/dev/null || echo "   (命名空间不存在 —— 驱动未安装)"
want "kubelet-plugin 1/1 Running；CrashLoopBackOff 多半是 WSL 驱动路径问题，先看 kubectl logs"

say "示例负载"
"$KUBECTL" -n "$NS" get resourceclaims 2>/dev/null || echo "   (命名空间不存在 —— 示例未部署)"
"$KUBECTL" -n "$NS" get pods -o wide 2>/dev/null || true
want "claim 已 allocated；dra-probe / bridge-probe 落在 GPU 节点并 Running"

say "tahrs 侧观察"
printf '   expected: 调度器日志出现 DRA 账本告警，指标 tahrs_gpu_ledger_blind_total 增长\n'
printf '   命令：kubectl -n tahrs-system logs deploy/tahrs-scheduler --tail=200 | grep -i dra\n'
printf '   注意：调度器必须带 --v>=2 才容易看到告警；指标端口按部署时配置。\n'
