#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: ecr-public-image-digest.sh --repository <name> --tag <tag>
EOF
  exit 2
}

repository=
tag=

while (($# > 0)); do
  case $1 in
    --repository)
      repository=${2-}
      shift 2
      ;;
    --tag)
      tag=${2-}
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ -n $repository && -n $tag ]] || usage

if response=$(aws ecr-public describe-images \
  --region us-east-1 \
  --repository-name "$repository" \
  --image-ids "imageTag=$tag" \
  --output json 2>&1); then
  digest=$(jq -er '.imageDetails[0].imageDigest' <<<"$response") || {
    printf 'ECR Public lookup returned no image digest for %s:%s\n' "$repository" "$tag" >&2
    exit 1
  }
  printf '%s\n' "$digest"
  exit 0
fi

if grep -Fq 'ImageNotFoundException' <<<"$response"; then
  exit 3
fi

printf '%s\n' "$response" >&2
exit 1
