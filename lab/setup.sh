#!/usr/bin/env bash
#
# Stand up a Spinnaker lab on an existing Kubernetes cluster, for developing and
# end-to-end testing sc.
#
# It deploys the upstream kustomize base (the officially supported install path
# since Halyard was deprecated in 2026.1.0), pinned to a release tag, then layers
# on the two overlays in lab/overlays/ that the operator plane and the E2E suite
# need.
#
# Usage:
#   lab/setup.sh                                  # defaults below
#   SPINNAKER_HOST=spinnaker.example.com lab/setup.sh
#   SPINNAKER_VERSION=2026.2.3 lab/setup.sh
#   lab/setup.sh --delete                         # remove the namespace
#
# Requirements: kubectl with kustomize (>= 1.21), git, and a cluster with ~16GB
# of memory and 6 cores free. The upstream base grants Spinnaker cluster-admin
# and sets a default password, so use a private cluster only.
set -euo pipefail

VERSION="${SPINNAKER_VERSION:-2026.3.0}"
NAMESPACE="${SPINNAKER_NAMESPACE:-spinnaker}"
WORKDIR="${SPINNAKER_WORKDIR:-${HOME}/spinnaker-lab}"
SRCDIR="${SPINNAKER_SRCDIR:-${HOME}/spinnaker-src}"
INGRESS_CLASS="${SPINNAKER_INGRESS_CLASS:-traefik}"
# nip.io resolves <anything>.<ip>.nip.io to <ip>, which avoids editing DNS or
# /etc/hosts for a lab.
HOST="${SPINNAKER_HOST:-}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

if [[ "${1:-}" == "--delete" ]]; then
  log "Deleting namespace ${NAMESPACE}"
  kubectl delete namespace "${NAMESPACE}" --ignore-not-found
  exit 0
fi

command -v kubectl >/dev/null || die "kubectl is required"
command -v git >/dev/null || die "git is required"
kubectl version --client >/dev/null || die "kubectl cannot run"

if [[ -z "${HOST}" ]]; then
  # Prefer the ingress controller's external address; fall back to the node IP.
  ip="$(kubectl get svc -A -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.status.loadBalancer.ingress[0].ip}{"\n"}{end}' 2>/dev/null | grep -m1 -E '^[0-9]+\.' || true)"
  if [[ -z "${ip}" ]]; then
    ip="$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' | awk '{print $1}')"
  fi
  [[ -n "${ip}" ]] || die "could not determine an ingress address; set SPINNAKER_HOST"
  HOST="spinnaker.${ip}.nip.io"
fi

log "Spinnaker ${VERSION} into namespace ${NAMESPACE}, reachable at http://${HOST}/"

# ---------------------------------------------------------------------------
# Fetch the upstream kustomize base
# ---------------------------------------------------------------------------
# The standalone spinnaker/spinnaker-kustomize repo was archived into the
# monorepo, so the base now lives at spinnaker/spinnaker//spinnaker-kustomize.
if [[ ! -d "${SRCDIR}/.git" ]]; then
  log "Cloning the Spinnaker monorepo into ${SRCDIR} (shallow)"
  git clone --depth 1 https://github.com/spinnaker/spinnaker.git "${SRCDIR}"
else
  log "Reusing the monorepo clone at ${SRCDIR}"
fi
[[ -d "${SRCDIR}/spinnaker-kustomize" ]] || die "${SRCDIR}/spinnaker-kustomize is missing"

log "Preparing ${WORKDIR}"
rm -rf "${WORKDIR}"
cp -r "${SRCDIR}/spinnaker-kustomize" "${WORKDIR}"
cd "${WORKDIR}"

# ---------------------------------------------------------------------------
# Pin the version and point every host reference at this lab
# ---------------------------------------------------------------------------
# The base ships newTag: main-latest, which is a moving target.
log "Pinning images to ${VERSION}"
sed -i.bak "s/newTag: main-latest/newTag: ${VERSION}/" kustomization.yml && rm -f kustomization.yml.bak

# The hostname appears in three places and all three must agree, or logins
# redirect to the wrong origin and Deck calls the wrong API base.
log "Setting the hostname to ${HOST}"
sed -i.bak "s|http://example.com/|http://${HOST}/|" base/spinnaker.yml && rm -f base/spinnaker.yml.bak
sed -i.bak "s|http://example.com/api/v1|http://${HOST}/api/v1|" base/deck/files/settings.js && rm -f base/deck/files/settings.js.bak
sed -i.bak "s/host: example.com/host: ${HOST}/" ingress.yaml && rm -f ingress.yaml.bak
sed -i.bak "s|^#  ingressClassName: nginx|  ingressClassName: ${INGRESS_CLASS}|" ingress.yaml && rm -f ingress.yaml.bak

# ---------------------------------------------------------------------------
# Layer on the lab overlays
# ---------------------------------------------------------------------------
LAB_OVERLAYS="$(cd "$(dirname "${BASH_SOURCE[0]}")/overlays" && pwd)"
log "Applying the actuator overlay (enables the sc operator plane)"
cp "${LAB_OVERLAYS}/spinnaker-local.yml" overlays/config/files/spinnaker-local.yml
log "Applying the orca overlay (failedStages endpoint, webhook URL restrictions)"
cat "${LAB_OVERLAYS}/orca-local.yml" >> overlays/config/files/orca-local.yml

# ---------------------------------------------------------------------------
# Deploy
# ---------------------------------------------------------------------------
log "Rendering manifests"
kubectl kustomize . -o spinnaker.yaml
log "Applying $(grep -c '^kind:' spinnaker.yaml) objects"
kubectl apply -f spinnaker.yaml

log "Waiting for rollouts (pods crash-loop until MySQL and Redis accept connections)"
for d in clouddriver front50 orca gate igor echo fiat rosco kayenta deck; do
  kubectl -n "${NAMESPACE}" rollout status "deploy/${d}" --timeout=10m || true
done

cat <<EOF

$(log "Lab ready")

  Deck UI    http://${HOST}/
  Gate API   http://${HOST}/api/v1
  Login      admin / spinnakerSaysYouShouldChangeYourPassword
             (set in overlays/config/files/gate-local.yml — change it)

Point sc at it:

  sc login --gate http://${HOST}/api/v1 \\
      --user admin --password spinnakerSaysYouShouldChangeYourPassword \\
      --context-name lab --save-namespace ${NAMESPACE}

  sc doctor
  sc svc list

Run the end-to-end suite against it:

  export SC_E2E=1 SC_E2E_GATE=http://${HOST}/api/v1
  export SC_E2E_USER=admin SC_E2E_PASSWORD=spinnakerSaysYouShouldChangeYourPassword
  export SC_E2E_KUBECONFIG="\${KUBECONFIG:-\$HOME/.kube/config}"
  export SC_E2E_WEBHOOK_URL=http://front50.${NAMESPACE}.svc.cluster.local:8080/health
  make e2e

Working copy: ${WORKDIR}
Tear down:    lab/setup.sh --delete
EOF
