#!/usr/bin/env bash
#
# Local end-to-end check: one real cluster, several fake nodes.
#
# The cluster can be any cluster you hold cluster-admin on (k3s on the same
# machine is enough). KWOK creates the fake nodes and plays their kubelets;
# the API server, the scheduling framework and the real tahrs binary all run
# unmodified, so PreFilter, PreScore, Score, the NodeAffinity addedAffinity,
# taints and binding are exercised for real.
#
#   ./test/local/run.sh          run and clean up
#   KEEP=1 ./test/local/run.sh   leave the namespace, nodes and logs in place
#
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
HERE="$ROOT/test/local"
NAMESPACE=${NAMESPACE:-tahrs-local-test}
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}
KWOK_PORT=${KWOK_PORT:-10247}
SCHED_PORT=${SCHED_PORT:-10279}
KEEP=${KEEP:-0}
CACHE=${CACHE:-$HOME/.cache/tahrs-local}
WORK=${WORK:-$(mktemp -d "/tmp/tahrs-local.XXXXXX")}
mkdir -p "$WORK"

log() { printf '\n== %s\n' "$*"; }
die() { printf 'run.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing '$1'${2:+ ($2)}"; }

cleanup() {
  local code=$?
  if [[ -n ${SCHED_PID:-} ]]; then kill "$SCHED_PID" 2>/dev/null || true; fi
  if [[ ${STARTED_KWOK:-0} == 1 && -n ${KWOK_PID:-} ]]; then kill "$KWOK_PID" 2>/dev/null || true; fi
  if [[ $KEEP == 1 ]]; then
    printf '\nfake nodes and namespace %s kept; logs under %s\n' "$NAMESPACE" "$WORK"
  else
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
kubectl apply -f "$HERE/nodes.yaml" >/dev/null
for node in $(kubectl get nodes -l tahrs.dev/local-test=true -o name); do
  kubectl wait --for=condition=Ready "$node" --timeout=60s >/dev/null
done
kubectl get nodes -l tahrs.dev/local-test=true -L scheduling.example.com/pool

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

fail=0
log "result"
if [[ $cpu_node == tahrs-fake-cpu-* ]]; then
  printf 'PASS  tahrs-cpu          -> %s\n' "$cpu_node"
else
  printf 'FAIL  tahrs-cpu          -> %s (want a tahrs-fake-cpu-* node)\n' "${cpu_node:-unscheduled}"; fail=1
fi
if [[ $gpu_node == tahrs-fake-gpu-* ]]; then
  printf 'PASS  tahrs-gpu          -> %s\n' "$gpu_node"
else
  printf 'FAIL  tahrs-gpu          -> %s (want a tahrs-fake-gpu-* node)\n' "${gpu_node:-unscheduled}"; fail=1
fi
if [[ -z $bad_node && $events == *"unknown profile"* ]]; then
  printf 'PASS  tahrs-bad-profile  -> still Pending, rejected in PreFilter\n'
else
  printf 'FAIL  tahrs-bad-profile  -> node=%s events=%s\n' "${bad_node:-none}" "${events:-none}"; fail=1
fi

if [[ $KEEP == 1 ]]; then
  printf '\nscheduler log: %s\n' "$WORK/scheduler.log"
fi

exit $fail
