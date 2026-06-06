#!/usr/bin/env bash
# Deploy the proxy service to every Cloud Run region.
# Usage: ./deploy.sh
# Requires: gcloud authed, PROJECT set, secret 'proxy-secret' in Secret Manager.

set -euo pipefail

PROJECT="${PROJECT:-schniffer}"
SERVICE="${SERVICE:-proxyrequest}"
SECRET_NAME="${SECRET_NAME:-proxy-secret}"
REPO="${REPO:-proxy}"
# Artifact Registry repo lives in this single region; images are global.
AR_REGION="${AR_REGION:-us-central1}"
IMAGE_TAG="${IMAGE_TAG:-$(date -u +%Y%m%d-%H%M%S)}"
IMAGE="${AR_REGION}-docker.pkg.dev/${PROJECT}/${REPO}/${SERVICE}:${IMAGE_TAG}"

# All Cloud Run regions (https://cloud.google.com/run/docs/locations).
REGIONS=(
  africa-south1
  asia-east1 asia-east2
  asia-northeast1 asia-northeast2 asia-northeast3
  asia-south1 asia-south2
  asia-southeast1 asia-southeast2
  australia-southeast1 australia-southeast2
  europe-central2
  europe-north1 europe-north2
  europe-southwest1
  europe-west1 europe-west2 europe-west3 europe-west4
  europe-west6 europe-west8 europe-west9
  europe-west10 europe-west12
  me-central1 me-central2 me-west1
  northamerica-northeast1 northamerica-northeast2 northamerica-south1
  southamerica-east1 southamerica-west1
  us-central1 us-east1 us-east4 us-east5 us-south1
  us-west1 us-west2 us-west3 us-west4
)

echo "== Project: $PROJECT, Service: $SERVICE, Image: $IMAGE"

# Ensure Artifact Registry repo exists (idempotent).
if ! gcloud artifacts repositories describe "$REPO" \
    --location="$AR_REGION" --project="$PROJECT" >/dev/null 2>&1; then
  echo "Creating Artifact Registry repo '$REPO' in $AR_REGION"
  gcloud artifacts repositories create "$REPO" \
    --repository-format=docker \
    --location="$AR_REGION" \
    --project="$PROJECT"
fi

# Resolve secret once at deploy time so it's baked as env var (no runtime Secret Manager calls).
SECRET_VALUE="$(gcloud secrets versions access latest --secret="$SECRET_NAME" --project="$PROJECT")"
if [[ -z "$SECRET_VALUE" ]]; then
  echo "ERROR: could not read secret '$SECRET_NAME'" >&2
  exit 1
fi

# Build once via Cloud Build, push to AR.
echo "== Building image with Cloud Build..."
gcloud builds submit --tag="$IMAGE" --project="$PROJECT" --quiet

# Deploy to all regions in parallel (background) — much faster than serial.
echo "== Deploying to ${#REGIONS[@]} regions in parallel..."
PIDS=()
LOGDIR="$(mktemp -d)"
for region in "${REGIONS[@]}"; do
  (
    gcloud run deploy "$SERVICE" \
      --image="$IMAGE" \
      --project="$PROJECT" \
      --region="$region" \
      --allow-unauthenticated \
      --cpu=1 \
      --memory=256Mi \
      --max-instances=2 \
      --timeout=30 \
      --concurrency=80 \
      --set-env-vars="PROXY_SECRET=${SECRET_VALUE},CLOUD_RUN_REGION=${region}" \
      --quiet >"${LOGDIR}/${region}.log" 2>&1 \
      && echo "  ✓ ${region}" \
      || echo "  ✗ ${region} (see ${LOGDIR}/${region}.log)"
  ) &
  PIDS+=($!)
done

for pid in "${PIDS[@]}"; do wait "$pid" || true; done

echo "== Logs in $LOGDIR"
echo "== Listing deployed services..."
for region in "${REGIONS[@]}"; do
  url=$(gcloud run services describe "$SERVICE" --region="$region" --project="$PROJECT" \
    --format="value(status.url)" 2>/dev/null || true)
  if [[ -n "$url" ]]; then
    echo "$region $url"
  fi
done
