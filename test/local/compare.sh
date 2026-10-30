#!/usr/bin/env bash
#
# Compare the tahrs score plugin with the cluster's built-in scheduler on the
# same fake-node fixture.
#
# Both phases run against exactly the same nodes, the same pod spec and the same
# baseline empty cluster; only the scheduler that binds the pods differs. Phase
# one starts the tahrs binary as a second scheduler; phase two leaves the pods
# to the built-in scheduler that ships with the cluster.
#
#   ./test/local/compare.sh
#   NODEFILE=$PWD/test/local/nodes-200.yaml PODS=300 ./test/local/compare.sh
#   TOLERATE_GPU=0 ./test/local/compare.sh    keep both schedulers off GPU nodes
#   PODS=600 ./test/local/compare.sh          more pods than one small class holds
#   KEEP=1 ./test/local/compare.sh            leave nodes and placements in place
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
import pathlib
import sys

import yaml

src, work = sys.argv[1], sys.argv[2]
data = yaml.safe_load(open(src))["data"]
base = yaml.safe_load(data["kube-scheduler-config.yaml"])
base["clientConnection"] = {"kubeconfig": f"{work}/kubeconfig"}
for profile in base["profiles"]:
    for plugin in profile.get("pluginConfig", []):
        if plugin["name"] == "tahrs":
            plugin["args"]["configPath"] = f"{work}/tahrs-config.yaml"
pathlib.Path(f"{work}/tahrs-config.yaml").write_text(data["tahrs-config.yaml"])
pathlib.Path(f"{work}/kube-scheduler-config.yaml").write_text(yaml.safe_dump(base, sort_keys=False))
PY

render_pods() {
  local scheduler=$1
  python3 - "$PODS" "$scheduler" "$TOLERATE_GPU" >"$WORK/pods-$scheduler.yaml" <<'PY'
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
render_pods tahrs-scheduler
render_pods "$NATIVE_SCHEDULER"

# One phase: fresh namespace, apply every pod, wait until each has a node, dump
# the placement, then clear the pods again so the next phase starts from the
# same empty cluster.
run_phase() {
  local tag=$1 scheduler=$2
  kubectl delete namespace "$NAMESPACE" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  kubectl create namespace "$NAMESPACE" >/dev/null
  local start
  start=$(date +%s%N)
  kubectl -n "$NAMESPACE" apply -f "$WORK/pods-$scheduler.yaml" >/dev/null
  local scheduled=0 deadline=$((SECONDS + 600))
  while ((SECONDS < deadline)); do
    scheduled=$(kubectl -n "$NAMESPACE" get pods -l app=tahrs-compare \
      -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep -c . || true)
    [[ $scheduled -ge $PODS ]] && break
    sleep 0.5
  done
  local end
  end=$(date +%s%N)
  local ms=$(( (end - start) / 1000000 ))
  kubectl -n "$NAMESPACE" get pods -l app=tahrs-compare \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\n"}{end}' |
    grep -v $'\t$' >"$WORK/placements-$tag.tsv" || true
  printf '      %s: %s/%s scheduled in %s ms\n' "$tag" "$scheduled" "$PODS" "$ms"
  echo "$scheduled $ms" >"$WORK/timing-$tag.txt"
  kubectl -n "$NAMESPACE" delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
  [[ $scheduled -ge $PODS ]] || die "$tag scheduled only $scheduled/$PODS pods"
}

log "phase 1: tahrs-scheduler"
"$WORK/tahrs-scheduler" --config="$WORK/kube-scheduler-config.yaml" \
  --secure-port="$SCHED_PORT" --leader-elect=false --v=3 \
  >"$WORK/scheduler.log" 2>&1 &
SCHED_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$SCHED_PID" 2>/dev/null || { cat "$WORK/scheduler.log"; die "scheduler exited"; }
  grep -q 'Starting Kubernetes Scheduler' "$WORK/scheduler.log" && break
  sleep 1
done
run_phase tahrs tahrs-scheduler
kill "$SCHED_PID" 2>/dev/null || true
wait "$SCHED_PID" 2>/dev/null || true
unset SCHED_PID

log "phase 2: built-in $NATIVE_SCHEDULER"
run_phase native "$NATIVE_SCHEDULER"

log "comparison"
python3 - "$WORK" "$PODS" "$NATIVE_SCHEDULER" <<'PY'
import collections
import json
import statistics
import sys

work, pods, native = sys.argv[1], int(sys.argv[2]), sys.argv[3]

nodes = {}
for item in json.load(open(f"{work}/nodes.json"))["items"]:
    labels = item["metadata"]["labels"]
    alloc = item["status"]["allocatable"]
    nodes[item["metadata"]["name"]] = {
        "class": labels.get("scheduling.example.com/class", "?"),
        "cpu": int(alloc["cpu"]),
        "gpu": int(alloc.get("nvidia.com/gpu", 0)),
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

def timing(tag):
    scheduled, ms = open(f"{work}/timing-{tag}.txt").read().split()
    return int(ms)

def summarize(tag):
    placed = placements(tag)
    per_node = collections.Counter(placed.values())
    counts = list(per_node.values())
    by_class = collections.Counter()
    cpu_by_node = collections.Counter()
    for node, count in per_node.items():
        info = nodes.get(node, {"class": "?", "cpu": 1, "gpu": 0})
        by_class[f"{info['class']}-{info['cpu']}c"] += count
        cpu_by_node[node] = count
    on_gpu = sum(c for n, c in per_node.items() if nodes.get(n, {}).get("gpu", 0) > 0)
    utilisation = [cpu_by_node.get(n, 0) / i["cpu"] for n, i in nodes.items()]
    mean = statistics.mean(utilisation) if utilisation else 0
    cv = statistics.pstdev(utilisation) / mean if mean else 0.0
    idle = sum(1 for n in nodes if n not in per_node)
    return {
        "placed": len(placed),
        "ms": timing(tag),
        "used": len(per_node),
        "idle": idle,
        "mean": statistics.mean(counts) if counts else 0,
        "sd": statistics.pstdev(counts) if len(counts) > 1 else 0,
        "max": max(counts) if counts else 0,
        "min": min(counts) if counts else 0,
        "gpu_pods": on_gpu,
        "by_class": by_class,
        "cv": cv,
        "per_node": per_node,
    }

def cls_list(counter):
    return ", ".join(f"{k}={v}" for k, v in sorted(counter.items()))

rows = [("tahrs", summarize("tahrs")), ("native", summarize("native"))]
w = (22, 12, 12)
print(f"{'metric':<{w[0]}}{'tahrs':>{w[1]}}{'native':>{w[2]}}")
print("-" * sum(w))
def line(label, a, b):
    print(f"{label:<{w[0]}}{a:>{w[1]}}{b:>{w[2]}}")

line("pods placed", rows[0][1]["placed"], rows[1][1]["placed"])
line("wall clock to bind (ms)", rows[0][1]["ms"], rows[1][1]["ms"])
line("nodes used", rows[0][1]["used"], rows[1][1]["used"])
line("nodes left idle", rows[0][1]["idle"], rows[1][1]["idle"])
line("pods per used node (mean)", f"{rows[0][1]['mean']:.2f}", f"{rows[1][1]['mean']:.2f}")
line("pods per used node (sd)", f"{rows[0][1]['sd']:.2f}", f"{rows[1][1]['sd']:.2f}")
line("busiest node (pods)", rows[0][1]["max"], rows[1][1]["max"])
line("quietest used node (pods)", rows[0][1]["min"], rows[1][1]["min"])
line("pods on GPU nodes", rows[0][1]["gpu_pods"], rows[1][1]["gpu_pods"])
line("CPU utilisation CV", f"{rows[0][1]['cv']:.3f}", f"{rows[1][1]['cv']:.3f}")
print()
print("pods per node class")
for tag, row in rows:
    print(f"  {tag:<8}{cls_list(row['by_class'])}")
print()
print("busiest nodes")
for tag, row in rows:
    top = ", ".join(f"{n}={c}" for n, c in row["per_node"].most_common(6))
    print(f"  {tag:<8}{top}")
PY

if [[ $KEEP == 1 ]]; then
  printf '\nlogs: %s\n' "$WORK"
fi
