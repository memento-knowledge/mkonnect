#!/usr/bin/env bash
#
# Asserts that what the project ships holds nothing that is not meant to ship: no test code, test
# fixtures, test-rig files, scripts, source files or VCS state. It only checks; it changes nothing.
#
#   check-artifact-contents.sh image <image> <binary-path>
#       The final filesystem of a container image (docker create + docker export): nothing forbidden
#       anywhere in it, no top-level directory beyond the operating system's own, the binary present as
#       the one file the build adds, and /data present and empty.
#   check-artifact-contents.sh listing <file> <binary-path>
#       The same checks on a saved `tar -t` listing of an image filesystem (used by the tests).
#   check-artifact-contents.sh dockerfile <Dockerfile> <source>...
#       The final stage of a Dockerfile copies only the given paths, and only from the builder stage.
#   check-artifact-contents.sh chart <package.tgz>
#       A packaged Helm chart holds only the chart's own files.
#   check-artifact-contents.sh assets <dir> <chart-name> <chart-version>
#       A directory of release assets holds exactly the chart package, its checksum and, optionally,
#       release-metadata.json.
#
# The patterns are anchored on path segments, so an operating system's own "usr/bin/test" or
# "etc/profile.d/*.sh" is not mistaken for test code.

set -euo pipefail

usage() {
  sed -n '3,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

# Paths no artifact may contain, wherever they appear.
forbidden_re='(^|/)(\.git|\.github|\.claude|\.worktrees|testdata|scripts|docs|charts|cmd|internal)(/|$)|(^|/)\.git(ignore|attributes|modules)$|\.go$|(^|/)go\.(mod|sum)$|(^|/)Dockerfile[^/]*$|(^|/)replicarig[^/]*$|(^|/)RIG\.md$|_test\.[A-Za-z]+$'

# Top-level directories an image may have: the operating system's own, plus what the build adds.
system_top='.dockerenv bin boot data dev etc home lib lib64 media mnt opt proc root run sbin srv sys tmp usr var'

# One scratch directory and, for an image check, one container, both removed on exit. (A RETURN trap
# would outlive its function and see variables that are already out of scope, so there is none.)
workdir=$(mktemp -d)
container=
cleanup() {
  [[ -z $container ]] || docker rm "$container" >/dev/null 2>&1 || true
  rm -rf "$workdir"
}
trap cleanup EXIT

fail=0
report() {
  printf '%s\n' "$*" >&2
  fail=1
}

check_forbidden() {
  local listing=$1 what=$2 hits
  hits=$(grep -E "$forbidden_re" "$listing" || true)
  if [[ -n $hits ]]; then
    report "$what contains paths that must not be shipped:"
    printf '  %s\n' $hits >&2
  fi
}

check_listing() {
  local listing=$1 binary=${2#/} normalized="$workdir/normalized" top entries
  sed -e 's#^\./##' -e 's#/$##' "$listing" | grep -v '^\.\{0,1\}$' >"$normalized" || true
  [[ -s $normalized ]] || {
    printf 'the filesystem listing is empty\n' >&2
    return 1
  }

  check_forbidden "$normalized" "the image"

  while IFS= read -r top; do
    [[ $top == "$binary" ]] && continue
    case " $system_top " in
      *" $top "*) ;;
      *) report "unexpected top-level entry in the image: /$top" ;;
    esac
  done < <(cut -d/ -f1 "$normalized" | sort -u)

  grep -qx "$binary" "$normalized" || report "the image does not contain the binary /$binary"
  grep -qx 'data' "$normalized" || report "the image does not contain /data"
  entries=$(grep -c '^data/' "$normalized" || true)
  [[ $entries -eq 0 ]] || report "/data must ship empty, found $entries entries"
  return "$fail"
}

check_image() {
  local image=$1 binary=$2
  # The argument is never run (nothing is started); it only lets an image without a command be created.
  container=$(docker create "$image" unused)
  docker export "$container" | tar -t >"$workdir/image.lst"
  check_listing "$workdir/image.lst" "$binary"
}

# check_dockerfile verifies the COPY and ADD instructions of the last stage. Everything else the
# final stage does is a RUN of the base image's own tools and cannot bring in files from the build
# context.
check_dockerfile() {
  local file=$1 line instruction rest src found
  shift
  local allowed=("$@") in_final=false
  [[ -f $file ]] || {
    printf 'no such Dockerfile: %s\n' "$file" >&2
    return 1
  }
  local last_from
  last_from=$(grep -n -i '^FROM[[:space:]]' "$file" | tail -1 | cut -d: -f1)
  [[ -n $last_from ]] || {
    printf '%s has no FROM\n' "$file" >&2
    return 1
  }
  local n=0
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    ((n > last_from)) || continue
    instruction=$(printf '%s' "$line" | awk '{ print toupper($1) }')
    case $instruction in
      ADD) report "$file:$n: the final stage must not use ADD: $line" ;;
      COPY)
        in_final=true
        rest=${line#*[[:space:]]}
        if [[ $rest != *--from=builder* ]]; then
          report "$file:$n: the final stage may only COPY --from=builder: $line"
          continue
        fi
        # The source is the argument after the flags and before the destination.
        src=$(printf '%s' "$rest" | tr -s ' ' '\n' | grep -v '^--' | head -1)
        found=false
        for a in "${allowed[@]}"; do
          [[ $src == "$a" ]] && found=true
        done
        $found || report "$file:$n: the final stage copies $src, which is not one of: ${allowed[*]}"
        ;;
    esac
  done <"$file"
  $in_final || report "$file: the final stage copies nothing from the builder"
  return "$fail"
}

check_chart() {
  local pkg=$1 listing="$workdir/chart.lst" root
  [[ -f $pkg ]] || {
    printf 'no such chart package: %s\n' "$pkg" >&2
    return 1
  }
  tar -tzf "$pkg" >"$listing"
  root=$(cut -d/ -f1 "$listing" | sort -u)
  [[ $(printf '%s\n' "$root" | wc -l) -eq 1 && -n $root ]] || {
    printf 'the chart package must have exactly one top-level directory, found: %s\n' "$root" >&2
    return 1
  }
  # A chart, and a Helm test hook in particular, is not test code of this repository: anything under
  # a tests/ directory is reported so that it is decided on purpose.
  if grep -E "$forbidden_re|(^|/)tests?(/|\$)|\.sh\$" "$listing" | grep -v '/$' >"$listing.hits"; then
    report "the chart package contains paths that must not be shipped:"
    sed 's/^/  /' "$listing.hits" >&2
  fi
  local unexpected
  unexpected=$(grep -vE "^$root/(Chart\.yaml|values\.yaml|\.helmignore|templates/|templates/[A-Za-z0-9_.-]+\.(yaml|yml|tpl|txt))$" "$listing" || true)
  if [[ -n $unexpected ]]; then
    report "the chart package contains files that are not part of the chart layout:"
    printf '  %s\n' $unexpected >&2
  fi
  return "$fail"
}

check_assets() {
  local dir=$1 name=$2 version=$3 f base
  [[ -d $dir ]] || {
    printf 'no such directory: %s\n' "$dir" >&2
    return 1
  }
  local package="$name-$version.tgz" seen_package=false seen_checksum=false
  while IFS= read -r f; do
    base=${f#"$dir"/}
    case $base in
      "$package") seen_package=true ;;
      "$package.sha256") seen_checksum=true ;;
      release-metadata.json) ;;
      *) report "unexpected release asset: $base" ;;
    esac
  done < <(find "$dir" -mindepth 1 | sort)
  $seen_package || report "the release assets lack $package"
  $seen_checksum || report "the release assets lack $package.sha256"
  return "$fail"
}

(($# >= 1)) || usage
mode=$1
shift
case $mode in
  image)
    (($# == 2)) || usage
    check_image "$1" "$2"
    ;;
  listing)
    (($# == 2)) || usage
    check_listing "$1" "$2"
    ;;
  dockerfile)
    (($# >= 2)) || usage
    check_dockerfile "$@"
    ;;
  chart)
    (($# == 1)) || usage
    check_chart "$1"
    ;;
  assets)
    (($# == 3)) || usage
    check_assets "$1" "$2" "$3"
    ;;
  *) usage ;;
esac
printf 'ok: %s\n' "$mode"
