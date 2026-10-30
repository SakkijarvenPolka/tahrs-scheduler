#!/usr/bin/env bash
#
# Compare scoring directions on the same fake-node fixture.
#
# Every arm runs against exactly the same nodes, pod spec, arrival pattern and
# empty cluster. Only the scheduler differs:
#
#   tahrs   the tahrs binary with the shipped configuration:
#             score = alpha*fit + beta*pack
#   spread  the same binary, same profile name and same weights, but the profile
#             carries `packMode: spread`, so the second term is 1-pack:
#             score = alpha*fit + beta*(1-pack)
#   native  the built-in scheduler of the cluster (not a control arm: its plugin
#             set differs, it is only an external reference point)
#
#   ./test/local/compare.sh
#   NODEFILE=$PWD/test/local/nodes-200.yaml PODS=300 ./test/local/compare.sh
#   ARMS="tahrs spread" SERIAL=1 RUNS=2 ./test/local/compare.sh
#   TOLERATE_GPU=0 ./test/local/compare.sh    keep pods off GPU nodes entirely
#   KEEP=1 ./test/local/compare.sh            leave nodes and placements in place
#
# SERIAL=1 applies one pod, waits for its binding, then applies the next one, so
# the input sequence is deterministic. RUNS=2 repeats every arm and reports
# whether the placement map is identical; that is the determinism check.
#
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
HERE="$ROOT/test/local"
NAMESPACE=${NAMESPACE:-tahrs-compare}
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}
KWOK_PORT=${KWOK_PORT:-10347}
SCHED_PORT=${SCHED_PORT:-10391}
NODEFILE=${NODEFILE:-$HERE/nodes-200.yaml}
PODS=${PODS:-300}
ARMS=${ARMS:-"tahrs spread native"}
SERIAL=${SERIAL:-0}
RUNS=${RUNS:-1}
TOLERATE_GPU=${TOLERATE_GPU:-1}
NATIVE_SCHEDULER=${NATIVE_SCHEDULER:-default-scheduler}
KEEP=${KEEP:-0}
CACHE=${CACHE:-$HOME/.cache/tahrs-local}
WORK=${WORK:-$(mktemp -d "/tmp/tahrs-compare.XXXXXX")}
mkdir -p "$WORK"

log() { printf '\n== %s\n' "$*"; }
die() { printf 'compare.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing '$1'"; }

cleanup() {
  local code=$?
  if [[ -n ${SCHED_PID:-} ]]; then kill "$SCHED_PID" 2>/dev/null || true; fi
  if [[ -n ${SAMPLER_PID:-} ]]; then kill "$SAMPLER_PID" 2>/dev/null || true; fi
  if [[ ${STARTED_KWOK:-0} == 1 && -n ${KWOK_PID:-} ]]; then kill "$KWOK_PID" 2>/dev/null || true; fi
  if [[ $KEEP == 1 ]]; then
    printf '\nfake nodes, namespace %s and placements kept; logs under %s\n' "$NAMESPACE" "$WORK"
  else
    kubectl -n "$NAMESPACE" delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete node -l tahrs.dev/local-test=true --ignore-not-found >/dev/null 2>&1 || true
    rm -rf "$WORK"
  fi
  exit $code
}
trap cleanup EXIT

need kubectl
need go
need curl
need python3
python3 -c 'import yaml' 2>/dev/null || die "python3 needs PyYAML (pip install pyyaml)"

log "cluster"
kubectl version >/dev/null || die "kubectl cannot reach a cluster"
kubectl config view --raw --flatten >"$WORK/kubeconfig"
chmod 600 "$WORK/kubeconfig"

log "kwok $KWOK_VERSION"
mkdir -p "$CACHE"
case $(uname -m) in
  x86_64|amd64) kwok_asset=kwok-linux-amd64 ;;
  aarch64|arm64) kwok_asset=kwok-linux-arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
if [[ -z ${KWOK:-} ]]; then
  for candidate in "$(command -v kwok || true)" "$HOME/go/bin/kwok"; do
    if [[ -n $candidate && -x $candidate ]]; then KWOK=$candidate; break; fi
  done
fi
if [[ -z ${KWOK:-} ]]; then
  KWOK="$CACHE/kwok-$KWOK_VERSION"
  if [[ ! -x $KWOK ]]; then
    curl -fsSL -o "$KWOK.tmp" "https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK_VERSION/$kwok_asset"
    chmod +x "$KWOK.tmp" && mv "$KWOK.tmp" "$KWOK"
  fi
fi
[[ -x $KWOK ]] || die "cannot find or download kwok"

for asset in kwok.yaml stage-fast.yaml; do
  if [[ ! -f "$CACHE/$asset" ]]; then
    curl -fsSL -o "$CACHE/$asset.tmp" "https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK_VERSION/$asset" \
      && mv "$CACHE/$asset.tmp" "$CACHE/$asset"
  fi
done

python3 - "$CACHE/kwok.yaml" >"$WORK/kwok-crds.yaml" <<'PY'
import sys
import yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
yaml.safe_dump_all([d for d in docs if d.get("kind") == "CustomResourceDefinition"], sys.stdout)
PY

kubectl apply -f "$WORK/kwok-crds.yaml" >/dev/null
kubectl apply -f "$CACHE/stage-fast.yaml" >/dev/null

log "kwok controller"
"$KWOK" --kubeconfig="$WORK/kubeconfig" \
  --manage-nodes-with-annotation-selector='kwok.x-k8s.io/node=fake' \
  --enable-crds=Stage --cidr=10.244.0.0/16 --node-port="$KWOK_PORT" -v=INFO \
  >"$WORK/kwok.log" 2>&1 &
KWOK_PID=$!
STARTED_KWOK=1
sleep 3
kill -0 "$KWOK_PID" 2>/dev/null || { cat "$WORK/kwok.log"; die "kwok exited"; }

log "fake nodes"
[[ -f $NODEFILE ]] || die "node file not found: $NODEFILE"
node_count=$(grep -c '^kind: Node$' "$NODEFILE" || true)
[[ ${node_count:-0} -gt 0 ]] || die "no Node documents in $NODEFILE"
kubectl apply -f "$NODEFILE" >/dev/null
printf '      %s node documents from %s\n' "$node_count" "$(basename "$NODEFILE")"
deadline=$((SECONDS + 180))
ready=0
while ((SECONDS < deadline)); do
  ready=$(kubectl get nodes -l tahrs.dev/local-test=true \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' |
    grep -c '^True$' || true)
  [[ $ready -ge $node_count ]] && break
  sleep 1
done
[[ $ready -ge $node_count ]] || die "only $ready/$node_count fake nodes became Ready"
printf '      %s fake nodes Ready\n' "$ready"
kubectl get nodes -l tahrs.dev/local-test=true -o json >"$WORK/nodes.json"

log "build and configuration"
(cd "$ROOT" && go build -trimpath -o "$WORK/tahrs-scheduler" ./cmd/tahrs-scheduler)
python3 - "$ROOT/deploy/base/configmap.yaml" "$WORK" <<'PY'
import copy
import pathlib
import sys

import yaml

src, work = sys.argv[1], sys.argv[2]
data = yaml.safe_load(open(src))["data"]
scheduler = yaml.safe_load(data["kube-scheduler-config.yaml"])
setting = yaml.safe_load(data["tahrs-config.yaml"])
scheduler["clientConnection"] = {"kubeconfig": f"{work}/kubeconfig"}


def write_pair(setting_obj, name):
    for profile in scheduler["profiles"]:
        for plugin in profile.get("pluginConfig", []):
            if plugin["name"] == "tahrs":
                plugin["args"]["configPath"] = f"{work}/{name}.yaml"
    pathlib.Path(f"{work}/{name}.yaml").write_text(yaml.safe_dump(setting_obj, sort_keys=False))
    pathlib.Path(f"{work}/kube-scheduler-{name}.yaml").write_text(
        yaml.safe_dump(scheduler, sort_keys=False))


write_pair(setting, "tahrs-config")

spread = copy.deepcopy(setting)
default = spread["defaultProfile"]
spread["profiles"][default]["packMode"] = "spread"
write_pair(spread, "spread-config")
PY

render_pods() {
  local scheduler=$1 out=$2
  python3 - "$PODS" "$scheduler" "$TOLERATE_GPU" >"$out" <<'PY'
import sys

import yaml

count, scheduler, tolerate = int(sys.argv[1]), sys.argv[2], sys.argv[3] == "1"
docs = []
for index in range(count):
    spec = {
        "schedulerName": scheduler,
        "containers": [{
            "name": "pause",
            "image": "registry.k8s.io/pause:3.10",
            "resources": {"requests": {"cpu": "1", "memory": "2Gi"}},
        }],
    }
    if tolerate:
        spec["tolerations"] = [{
            "key": "scheduling.example.com/dedicated",
            "operator": "Equal",
            "value": "tahrs",
            "effect": "NoSchedule",
        }]
    docs.append({
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": f"cmp-{index + 1:04d}",
            "labels": {"app": "tahrs-compare"},
        },
        "spec": spec,
    })
sys.stdout.write("---\n".join(yaml.safe_dump(doc, sort_keys=False) for doc in docs))
PY
}

render_pods "$NATIVE_SCHEDULER" "$WORK/pods-native.yaml"
render_pods tahrs-scheduler "$WORK/pods-tahrs.yaml"
cp "$WORK/pods-tahrs.yaml" "$WORK/pods-spread.yaml"

arm_scheduler() {
  case $1 in
    tahrs|spread) printf 'tahrs-scheduler' ;;
    native) printf '%s' "$NATIVE_SCHEDULER" ;;
    *) die "unknown arm '$1' (want tahrs, spread or native)" ;;
  esac
}

arm_config() {
  case $1 in
    tahrs) printf '%s/kube-scheduler-tahrs-config.yaml' "$WORK" ;;
    spread) printf '%s/kube-scheduler-spread-config.yaml' "$WORK" ;;
    native) printf '' ;;
    *) die "unknown arm '$1'" ;;
  esac
}

start_scheduler() {
  local config=$1 arm=$2
  "$WORK/tahrs-scheduler" --config="$config" \
    --secure-port="$SCHED_PORT" --leader-elect=false --v=3 \
    >"$WORK/scheduler-$arm.log" 2>&1 &
  SCHED_PID=$!
  for _ in $(seq 1 30); do
    kill -0 "$SCHED_PID" 2>/dev/null || { cat "$WORK/scheduler-$arm.log"; die "scheduler for $arm exited"; }
    grep -q 'Starting Kubernetes Scheduler' "$WORK/scheduler-$arm.log" && return 0
    sleep 1
  done
  die "scheduler for $arm did not start"
}

stop_scheduler() {
  if [[ -n ${SCHED_PID:-} ]]; then
    kill "$SCHED_PID" 2>/dev/null || true
    wait "$SCHED_PID" 2>/dev/null || true
    unset SCHED_PID
  fi
}

# sample_rss writes the summed RSS (KiB) of the given PIDs every 300 ms until
# the marker file is removed.
sample_rss() {
  local out=$1 marker=$2
  shift 2
  : >"$out"
  while [[ -f $marker ]]; do
    local total=0 pid rss
    for pid in "$@"; do
      [[ -n $pid ]] || continue
      rss=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ' || true)
      [[ -n $rss ]] && total=$((total + rss))
    done
    printf '%s\n' "$total" >>"$out"
    sleep 0.3
  done
}

wait_for_binding() {
  local name=$1 deadline=$((SECONDS + 120))
  while ((SECONDS < deadline)); do
    local node
    node=$(kubectl -n "$NAMESPACE" get pod "$name" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
    [[ -n $node ]] && return 0
    sleep 0.05
  done
  return 1
}

apply_serially() {
  local scheduler=$1
  local i name
  for ((i = 1; i <= PODS; i++)); do
    name=$(printf 'cmp-%04d' "$i")
    python3 - "$name" "$scheduler" "$TOLERATE_GPU" <<'PY' | kubectl -n "$NAMESPACE" create -f - >/dev/null
import sys

import yaml

name, scheduler, tolerate = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
spec = {
    "schedulerName": scheduler,
    "containers": [{
        "name": "pause",
        "image": "registry.k8s.io/pause:3.10",
        "resources": {"requests": {"cpu": "1", "memory": "2Gi"}},
    }],
}
if tolerate:
    spec["tolerations"] = [{
        "key": "scheduling.example.com/dedicated",
        "operator": "Equal",
        "value": "tahrs",
        "effect": "NoSchedule",
    }]
sys.stdout.write(yaml.safe_dump({
    "apiVersion": "v1",
    "kind": "Pod",
    "metadata": {"name": name, "labels": {"app": "tahrs-compare"}},
    "spec": spec,
}, sort_keys=False))
PY
    wait_for_binding "$name" || die "pod $name never got a node in arm $scheduler"
  done
}

# run_phase applies one arm's pods to a fresh namespace, records placements,
# pod conditions, peak RSS and the namespace's final free capacity.
run_phase() {
  local tag=$1 podsfile=$2
  local scheduler
  scheduler=$(arm_scheduler "$tag")
  kubectl delete namespace "$NAMESPACE" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  kubectl create namespace "$NAMESPACE" >/dev/null

  local marker="$WORK/sampling-$tag"
  touch "$marker"
  sample_rss "$WORK/rss-$tag.txt" "$marker" "${SCHED_PID:-}" "$KWOK_PID" &
  SAMPLER_PID=$!

  local start
  start=$(date +%s%N)
  if [[ $SERIAL == 1 ]]; then
    apply_serially "$scheduler"
  else
    kubectl -n "$NAMESPACE" apply -f "$podsfile" >/dev/null
    local scheduled=0 deadline=$((SECONDS + 600))
    while ((SECONDS < deadline)); do
      scheduled=$(kubectl -n "$NAMESPACE" get pods -l app=tahrs-compare \
        -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep -c . || true)
      [[ $scheduled -ge $PODS ]] && break
      sleep 0.5
    done
  fi
  local end
  end=$(date +%s%N)

  rm -f "$marker"
  wait "$SAMPLER_PID" 2>/dev/null || true
  unset SAMPLER_PID

  kubectl -n "$NAMESPACE" get pods -l app=tahrs-compare -o json >"$WORK/pods-$tag.json"
  kubectl -n "$NAMESPACE" get pods -l app=tahrs-compare \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\n"}{end}' |
    grep -v $'\t$' >"$WORK/placements-$tag.tsv" || true
  local placed
  placed=$(wc -l <"$WORK/placements-$tag.tsv")
  local ms=$(((end - start) / 1000000))
  printf '      %s: %s/%s scheduled in %s ms\n' "$tag" "$placed" "$PODS" "$ms"
  echo "$placed $ms" >"$WORK/timing-$tag.txt"
  kubectl -n "$NAMESPACE" delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
  [[ $placed -ge $PODS ]] || die "$tag placed only $placed/$PODS pods"
}

run_arm() {
  local arm=$1 run=$2
  local config
  config=$(arm_config "$arm")
  log "arm $arm (run $run/$RUNS)"
  if [[ -n $config ]]; then
    start_scheduler "$config" "$arm"
  fi
  run_phase "$arm" "$WORK/pods-$arm.yaml"
  stop_scheduler
  if [[ $RUNS -gt 1 ]]; then
    cp "$WORK/placements-$arm.tsv" "$WORK/placements-$arm-run$run.tsv"
    cp "$WORK/pods-$arm.json" "$WORK/pods-$arm-run$run.json"
    cp "$WORK/rss-$arm.txt" "$WORK/rss-$arm-run$run.txt"
    cp "$WORK/timing-$arm.txt" "$WORK/timing-$arm-run$run.txt"
  fi
}

for arm in $ARMS; do
  for ((run = 1; run <= RUNS; run++)); do
    run_arm "$arm" "$run"
  done
done

log "comparison"
python3 - "$WORK" "$PODS" "$(printf '%s' "$ARMS")" "$RUNS" <<'PY'
import collections
import json
import statistics
import sys

work, pod_count, arms, runs = sys.argv[1], int(sys.argv[2]), sys.argv[3].split(), int(sys.argv[4])
GIB = 1 << 30


def quantity_cpu_milli(value):
    value = str(value)
    if value.endswith("m"):
        return int(value[:-1])
    return int(float(value) * 1000)


def quantity_bytes(value):
    value = str(value)
    for suffix, factor in (("Gi", GIB), ("Mi", 1 << 20), ("Ki", 1 << 10)):
        if value.endswith(suffix):
            return int(float(value[: -len(suffix)]) * factor)
    return int(float(value))


nodes = {}
for item in json.load(open(f"{work}/nodes.json"))["items"]:
    labels = item["metadata"]["labels"]
    alloc = item["status"]["allocatable"]
    nodes[item["metadata"]["name"]] = {
        "class": labels.get("scheduling.example.com/class", "?"),
        "cpu": quantity_cpu_milli(alloc.get("cpu", 0)),
        "mem": quantity_bytes(alloc.get("memory", 0)),
        "gpu": int(alloc.get("nvidia.com/gpu", 0) or 0),
    }


def placements(tag):
    out = {}
    for line in open(f"{work}/placements-{tag}.tsv"):
        line = line.rstrip("\n")
        if not line or "\t" not in line:
            continue
        name, node = line.split("\t", 1)
        out[name] = node
    return out


def peak_rss_mib(tag):
    path = f"{work}/rss-{tag}.txt"
    try:
        values = [int(line) for line in open(path) if line.strip()]
    except FileNotFoundError:
        return 0.0
    return max(values) / 1024 if values else 0.0


def summarize(tag):
    placed = placements(tag)
    per_node = collections.Counter(placed.values())
    counts = list(per_node.values())
    by_class = collections.Counter()
    for node, count in per_node.items():
        info = nodes.get(node, {"class": "?", "cpu": 1000})
        by_class[f"{info['class']}-{info['cpu'] // 1000}c"] += count

    # Free capacity left on each node after this arm, in the units the batch
    # pods use: one pod is 1 CPU / 2 GiB.
    fits_4c8g = 0
    fits_8c16g = 0
    for name, info in nodes.items():
        used = per_node.get(name, 0)
        free_cpu = info["cpu"] - used * 1000
        free_mem = info["mem"] - used * 2 * GIB
        if free_cpu >= 4000 and free_mem >= 8 * GIB:
            fits_4c8g += 1
        if free_cpu >= 8000 and free_mem >= 16 * GIB:
            fits_8c16g += 1

    on_gpu = sum(count for node, count in per_node.items() if nodes.get(node, {}).get("gpu", 0) > 0)
    utilisation = [per_node.get(name, 0) / max(1, info["cpu"] // 1000) for name, info in nodes.items()]
    mean = statistics.mean(utilisation) if utilisation else 0.0
    cv = statistics.pstdev(utilisation) / mean if mean else 0.0
    placed_count, ms = open(f"{work}/timing-{tag}.txt").read().split()
    rate = 1000.0 * int(placed_count) / int(ms) if int(ms) > 0 else 0.0
    return {
        "placed": int(placed_count),
        "ms": int(ms),
        "used": len(per_node),
        "idle": len(nodes) - len(per_node),
        "mean": statistics.mean(counts) if counts else 0.0,
        "sd": statistics.pstdev(counts) if len(counts) > 1 else 0.0,
        "max": max(counts) if counts else 0,
        "min": min(counts) if counts else 0,
        "gpu": on_gpu,
        "cv": cv,
        "rate": rate,
        "fits4": fits_4c8g,
        "fits8": fits_8c16g,
        "rss": peak_rss_mib(tag),
        "by_class": by_class,
        "per_node": per_node,
    }


data = {arm: summarize(arm) for arm in arms}
width = 24
print(f"{'metric':<{width}}" + "".join(f"{arm:>14}" for arm in arms))
print("-" * (width + 14 * len(arms)))
for label, key, fmt in [
    ("pods placed", "placed", "d"),
    ("wall clock to bind (ms)", "ms", "d"),
    ("nodes used", "used", "d"),
    ("nodes left idle", "idle", "d"),
    ("pods per used node (mean)", "mean", ".2f"),
    ("pods per used node (sd)", "sd", ".2f"),
    ("busiest node (pods)", "max", "d"),
    ("quietest used node (pods)", "min", "d"),
    ("pods on GPU nodes", "gpu", "d"),
    ("CPU utilisation CV", "cv", ".3f"),
    ("binding throughput (pods/s)", "rate", ".1f"),
    ("nodes that still fit 4C/8Gi", "fits4", "d"),
    ("nodes that still fit 8C/16Gi", "fits8", "d"),
    ("peak RSS sampled (MiB)", "rss", ".0f"),
]:
    row = "".join(f"{data[arm][key]:>14{fmt}}" for arm in arms)
    print(f"{label:<{width}}{row}")

print("\npods per node class")
for arm in arms:
    print(f"  {arm:<8}" + ", ".join(f"{k}={v}" for k, v in sorted(data[arm]["by_class"].items())))

print("\nbusiest nodes")
for arm in arms:
    top = ", ".join(f"{n}={c}" for n, c in data[arm]["per_node"].most_common(6))
    print(f"  {arm:<8}{top}")

if runs > 1:
    print("\ndeterminism (run1 vs run2 placement maps)")
    for arm in arms:
        first = {}
        second = {}
        for name, node in placements(f"{arm}-run1") .items():
            first[name] = node
        for name, node in placements(f"{arm}-run2").items():
            second[name] = node
        shared = set(first) & set(second)
        same = sum(1 for name in shared if first[name] == second[name])
        print(f"  {arm:<8}{same}/{len(shared)} pods on the same node")
PY

if [[ $KEEP == 1 ]]; then
  printf '\nlogs: %s\n' "$WORK"
fi
