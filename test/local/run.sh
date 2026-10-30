#!/usr/bin/env bash
#
# Local end-to-end check: one real cluster, many fake nodes.
#
# The cluster can be any cluster you hold cluster-admin on (k3s on the same
# machine is enough). KWOK creates the fake nodes and plays their kubelets;
# the API server, the scheduling framework and the real tahrs binary all run
# unmodified, so PreFilter, PreScore, Score, the NodeAffinity addedAffinity,
# taints and binding are exercised for real.
#
#   ./test/local/run.sh                                  four nodes, baseline
#   NODEFILE=$PWD/test/local/nodes-200.yaml ./test/local/run.sh
#                                                        two hundred nodes
#   NODEFILE=... PODS=300 ./test/local/run.sh             plus a batch workload
#   REAL_NODE=1 ./test/local/run.sh                      real node joins the pool
#   KEEP=1 ./test/local/run.sh                           leave everything in place
#
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
HERE="$ROOT/test/local"
NAMESPACE=${NAMESPACE:-tahrs-local-test}
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}
KWOK_PORT=${KWOK_PORT:-10247}
SCHED_PORT=${SCHED_PORT:-10279}
NODEFILE=${NODEFILE:-$HERE/nodes.yaml}
PODS=${PODS:-0}
REAL_NODE=${REAL_NODE:-0}
KEEP=${KEEP:-0}
CACHE=${CACHE:-$HOME/.cache/tahrs-local}
WORK=${WORK:-$(mktemp -d "/tmp/tahrs-local.XXXXXX")}
mkdir -p "$WORK"

real_node=""
REAL_NODE_LABELED=0

log() { printf '\n== %s\n' "$*"; }
die() { printf 'run.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing '$1'${2:+ ($2)}"; }

cleanup() {
  local code=$?
  if [[ -n ${SCHED_PID:-} ]]; then kill "$SCHED_PID" 2>/dev/null || true; fi
  if [[ ${STARTED_KWOK:-0} == 1 && -n ${KWOK_PID:-} ]]; then kill "$KWOK_PID" 2>/dev/null || true; fi
  if [[ $REAL_NODE_LABELED == 1 && -n $real_node ]]; then
    kubectl label node "$real_node" scheduling.example.com/pool- >/dev/null 2>&1 || true
  fi
  if [[ $KEEP == 1 ]]; then
    printf '\nfake nodes and namespace %s kept; logs under %s\n' "$NAMESPACE" "$WORK"
  else
    # Force the pods out first: a pod bound to a fake node has no kubelet left
    # to confirm its deletion, so it would keep the namespace in Terminating.
    kubectl -n "$NAMESPACE" delete pods --all --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete node -l tahrs.dev/local-test=true --ignore-not-found >/dev/null 2>&1 || true
    rm -rf "$WORK"
  fi
  exit $code
}
trap cleanup EXIT

need kubectl "https://kubernetes.io/docs/tasks/tools/"
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
    log "downloading kwok $KWOK_VERSION (about 116 MB, one time)"
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

if [[ $REAL_NODE == 1 ]]; then
  log "real node joins the pool"
  real_node=$(kubectl get nodes -l '!tahrs.dev/local-test' -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [[ -n $real_node ]] || die "REAL_NODE=1 but the cluster has no non-test node"
  kubectl label node "$real_node" scheduling.example.com/pool=heterogeneous --overwrite >/dev/null
  REAL_NODE_LABELED=1
  printf '      %s carries scheduling.example.com/pool=heterogeneous until exit\n' "$real_node"
fi

log "scheduler"
(cd "$ROOT" && go build -trimpath -o "$WORK/tahrs-scheduler" ./cmd/tahrs-scheduler)

# The local run reuses the shipped manifests' configuration instead of a
# hand-written copy: only clientConnection and the configPath are rewritten.
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

"$WORK/tahrs-scheduler" --config="$WORK/kube-scheduler-config.yaml" \
  --secure-port="$SCHED_PORT" --leader-elect=false --v=6 \
  >"$WORK/scheduler.log" 2>&1 &
SCHED_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$SCHED_PID" 2>/dev/null || { cat "$WORK/scheduler.log"; die "scheduler exited"; }
  grep -q 'Starting Kubernetes Scheduler' "$WORK/scheduler.log" && break
  sleep 1
done
grep -o 'configVersion=[^ ]*' "$WORK/scheduler.log" | head -1

log "workloads"
# A previous run may still be tearing its namespace down, and pods cannot be
# created into a Terminating namespace, so start from one that is really gone.
kubectl delete namespace "$NAMESPACE" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
kubectl create namespace "$NAMESPACE" >/dev/null
kubectl -n "$NAMESPACE" apply -f "$HERE/workloads.yaml" >/dev/null

for pod in tahrs-cpu tahrs-gpu; do
  kubectl -n "$NAMESPACE" wait --for=jsonpath='{.spec.nodeName}' "pod/$pod" --timeout=90s >/dev/null \
    || die "$pod was not scheduled, see $WORK/scheduler.log"
done

events=""
for _ in $(seq 1 20); do
  events=$(kubectl -n "$NAMESPACE" get events \
    --field-selector involvedObject.name=tahrs-bad-profile \
    -o jsonpath='{.items[*].message}' 2>/dev/null || true)
  [[ $events == *"unknown profile"* ]] && break
  sleep 2
done

cpu_node=$(kubectl -n "$NAMESPACE" get pod tahrs-cpu -o jsonpath='{.spec.nodeName}')
gpu_node=$(kubectl -n "$NAMESPACE" get pod tahrs-gpu -o jsonpath='{.spec.nodeName}')
bad_node=$(kubectl -n "$NAMESPACE" get pod tahrs-bad-profile -o jsonpath='{.spec.nodeName}')

# With REAL_NODE=1 the real node is a legitimate pool member, so it may win any
# of the two placements; without it, only fake nodes may be chosen.
in_class() {
  local node=$1 prefix=$2
  [[ $node == "$prefix"* ]] && return 0
  [[ $REAL_NODE == 1 && -n $real_node && $node == "$real_node" ]] && return 0
  return 1
}

fail=0
batch_ms=""
batch_ok=""
batch_scheduled=0

if [[ $PODS -gt 0 ]]; then
  log "batch workload ($PODS pods)"
  python3 - "$PODS" >"$WORK/batch.yaml" <<'PY'
import sys

import yaml

count = int(sys.argv[1])
docs = []
for index in range(count):
    docs.append({
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": f"tahrs-batch-{index + 1:04d}",
            "labels": {"app": "tahrs-batch"},
        },
        "spec": {
            "schedulerName": "tahrs-scheduler",
            "tolerations": [{
                "key": "scheduling.example.com/dedicated",
                "operator": "Equal",
                "value": "tahrs",
                "effect": "NoSchedule",
            }],
            "containers": [{
                "name": "pause",
                "image": "registry.k8s.io/pause:3.10",
                "resources": {"requests": {"cpu": "1", "memory": "2Gi"}},
            }],
        },
    })
sys.stdout.write("---\n".join(yaml.safe_dump(doc, sort_keys=False) for doc in docs))
PY
  start=$(date +%s%N)
  kubectl -n "$NAMESPACE" apply -f "$WORK/batch.yaml" >/dev/null
  deadline=$((SECONDS + 600))
  while ((SECONDS < deadline)); do
    batch_scheduled=$(kubectl -n "$NAMESPACE" get pods -l app=tahrs-batch \
      -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep -c . || true)
    [[ $batch_scheduled -ge $PODS ]] && break
    sleep 0.5
  done
  end=$(date +%s%N)
  batch_ms=$(( (end - start) / 1000000 ))
  if [[ $batch_scheduled -ge $PODS ]]; then
    batch_ok=1
  else
    batch_ok=0
  fi
  log "placement (top 10)"
  kubectl -n "$NAMESPACE" get pods -l app=tahrs-batch \
    -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' |
    grep -v '^$' | sort | uniq -c | sort -rn | head -10
fi

log "result"
if in_class "$cpu_node" "tahrs-fake-cpu-"; then
  printf 'PASS  tahrs-cpu          -> %s\n' "$cpu_node"
else
  printf 'FAIL  tahrs-cpu          -> %s (want a tahrs-fake-cpu-* node)\n' "${cpu_node:-unscheduled}"; fail=1
fi
if in_class "$gpu_node" "tahrs-fake-gpu-"; then
  printf 'PASS  tahrs-gpu          -> %s\n' "$gpu_node"
else
  printf 'FAIL  tahrs-gpu          -> %s (want a tahrs-fake-gpu-* node)\n' "${gpu_node:-unscheduled}"; fail=1
fi
if [[ -z $bad_node && $events == *"unknown profile"* ]]; then
  printf 'PASS  tahrs-bad-profile  -> still Pending, rejected in PreFilter\n'
else
  printf 'FAIL  tahrs-bad-profile  -> node=%s events=%s\n' "${bad_node:-none}" "${events:-none}"; fail=1
fi
if [[ $PODS -gt 0 ]]; then
  if [[ $batch_ok == 1 ]]; then
    printf 'PASS  batch              -> %d pods scheduled in %d ms (%d ms/pod)\n' \
      "$PODS" "$batch_ms" "$((batch_ms / PODS))"
  else
    printf 'FAIL  batch              -> %d/%d pods got a node after %d ms\n' \
      "$batch_scheduled" "$PODS" "$batch_ms"
    fail=1
  fi
fi

if [[ $KEEP == 1 ]]; then
  printf '\nscheduler log: %s\n' "$WORK/scheduler.log"
fi

exit $fail
