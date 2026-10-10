#!/usr/bin/env bash
# Bash is required by this fork's standalone release-tag tests on GitHub runners.
set -euo pipefail
export LC_ALL=C

VERSION_FILE=.github/rigel-version
VERSION_RE='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
TAG_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

fail() {
  echo "::error::$*" >&2
  return 1
}

read_version_file() {
  local file="$1"
  local -a lines=()
  if [[ ! -f "$file" ]]; then
    fail "Missing version source $file"
    return 1
  fi
  mapfile -t lines < "$file"
  if [[ "${#lines[@]}" -ne 1 || -z "${lines[0]}" ]]; then
    fail "$file must contain one bare version line"
    return 1
  fi
  version="${lines[0]}"
}

read_version_at() {
  local commit="$1"
  local -a lines=()
  if ! git cat-file -e "$commit:$VERSION_FILE" 2>/dev/null; then
    return 1
  fi
  mapfile -t lines < <(git show "$commit:$VERSION_FILE")
  if [[ "${#lines[@]}" -ne 1 || -z "${lines[0]}" ]]; then
    fail "$VERSION_FILE at $commit must contain one bare version line"
    return 1
  fi
  VERSION_VALUE="${lines[0]}"
}

validate_version() {
  if [[ ! "$1" =~ $VERSION_RE ]]; then
    fail "Invalid owned version '$1'; expected X.Y.Z with canonical non-negative integers"
    return 1
  fi
}

version_gt() {
  local left="$1" right="$2" left_part right_part index
  local -a left_parts right_parts
  IFS=. read -r -a left_parts <<< "$left"
  IFS=. read -r -a right_parts <<< "$right"
  for index in 0 1 2; do
    left_part="${left_parts[$index]}"
    right_part="${right_parts[$index]}"
    if [[ "${#left_part}" -gt "${#right_part}" ]]; then
      return 0
    elif [[ "${#left_part}" -lt "${#right_part}" ]]; then
      return 1
    elif [[ "$left_part" > "$right_part" ]]; then
      return 0
    elif [[ "$left_part" < "$right_part" ]]; then
      return 1
    fi
  done
  return 1
}

latest_tag() {
  local tag highest=
  while IFS= read -r tag; do
    [[ "$tag" =~ $TAG_RE ]] || continue
    if [[ -z "$highest" ]] || version_gt "${tag#v}" "${highest#v}"; then
      highest="$tag"
    fi
  done < <(git tag --list 'v*')
  printf '%s' "$highest"
}

check_version() {
  local diff_status highest version_source_exists=0
  if [[ -z "${BASE_SHA:-}" || -z "${HEAD_SHA:-}" ]]; then
    fail 'BASE_SHA and HEAD_SHA are required for the pull request version check'
    return 1
  fi
  if git diff --quiet "${BASE_SHA}...${HEAD_SHA}" -- "$VERSION_FILE"; then
    echo 'Version source unchanged; check skipped.'
    return 0
  else
    diff_status=$?
    if [[ "$diff_status" -ne 1 ]]; then
      fail 'Could not compare the pull request version source'
      return 1
    fi
  fi

  read_version_file "$VERSION_FILE"
  validate_version "$version"
  highest="$(latest_tag)"
  if git cat-file -e "${BASE_SHA}:$VERSION_FILE" 2>/dev/null; then
    version_source_exists=1
  fi
  if [[ -n "$highest" ]] && ! version_gt "$version" "${highest#v}"; then
    if [[ "$version_source_exists" -eq 0 && "$version" == "${highest#v}" ]]; then
      echo "Initial version source matches existing tag $highest; no tag will be minted."
      return 0
    fi
    fail "Invalid version '$version'; expected a version greater than $highest"
    return 1
  fi
  echo "Version $version is valid."
}

mint_tag() {
  local sha repo tag release_version highest target commit output status actual
  sha="${RELEASE_SHA:?RELEASE_SHA is required}"
  repo="${RELEASE_REPOSITORY:?RELEASE_REPOSITORY is required}"
  read_version_at "$sha" || { fail "Could not read $VERSION_FILE at $sha"; return 1; }
  release_version="$VERSION_VALUE"
  tag="v$release_version"

  if git show-ref --verify --quiet "refs/tags/$tag"; then
    echo "Tag $tag already exists; nothing to mint."
    return 0
  fi

  validate_version "$release_version"
  highest="$(latest_tag)"
  if [[ -n "$highest" ]] && ! version_gt "$release_version" "${highest#v}"; then
    fail "Invalid version '$release_version'; expected a version greater than $highest"
    return 1
  fi

  target=
  while IFS= read -r commit; do
    if ! read_version_at "$commit"; then
      break
    fi
    if [[ "$VERSION_VALUE" != "$release_version" ]]; then
      break
    fi
    target="$commit"
  done < <(git rev-list --first-parent "$sha")

  if [[ -z "$target" ]]; then
    fail "Could not find the first-parent commit that set $release_version"
    return 1
  fi

  if output="$(gh api "repos/$repo/git/refs" --method POST -f "ref=refs/tags/$tag" -f "sha=$target" 2>&1)"; then
    echo "Created $tag at $target."
    return 0
  else
    status=$?
  fi
  if [[ "$output" == *'Reference already exists'* && "$output" == *'422'* ]]; then
    actual="$(gh api "repos/$repo/git/ref/tags/$tag" --jq .object.sha)"
    if [[ "$actual" == "$target" ]]; then
      echo "Tag $tag was created concurrently at $target."
      return 0
    fi
    fail "Tag $tag already exists at $actual, expected $target"
    return 1
  fi
  fail "Could not create $tag: $output"
  return "$status"
}

resolve_tag() {
  local tag="$2" sha="$3" version
  if [[ ! "$tag" =~ $TAG_RE ]]; then
    fail "Invalid release tag '$tag'; expected vX.Y.Z"
    return 1
  fi
  if ! git rev-parse --verify --quiet origin/main >/dev/null; then
    fail 'origin/main is missing; cannot verify release ancestry'
    return 1
  fi
  if ! git merge-base --is-ancestor "$sha" origin/main; then
    fail "$sha is not reachable from origin/main"
    return 1
  fi
  if ! read_version_at "$sha"; then
    fail "Could not read $VERSION_FILE at $sha"
    return 1
  fi
  version="$VERSION_VALUE"
  validate_version "$version"
  if [[ "$tag" != "v$version" ]]; then
    fail "Tag $tag does not match $VERSION_FILE at $sha (v$version)"
    return 1
  fi
  echo "Resolved $tag at $sha."
}

case "${1:-}" in
  check)
    check_version
    ;;
  mint)
    mint_tag
    ;;
  resolve)
    resolve_tag "$@"
    ;;
  *)
    fail "Usage: $0 {check|mint|resolve <tag> <sha>}"
    ;;
esac
