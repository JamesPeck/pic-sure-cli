#!/usr/bin/env bash
# Moves the e2e workflow's build caches between the Docker daemon and a
# directory that actions/cache saves and restores:
#
#   e2e-cache.sh load-images DIR   docker load DIR/images.tar, if there
#   e2e-cache.sh save-images DIR LIST
#                                  docker save the images LIST names, one a line
#   e2e-cache.sh load-maven DIR    DIR/m2.tar into the pic-sure-m2 volume
#   e2e-cache.sh save-maven DIR    the pic-sure-m2 volume into DIR/m2.tar
#
# The images carry the source labels the build checks, so restored images
# for the release's commits mean init builds nothing. The Maven volume
# only helps when something is built.

set -euo pipefail

volume=pic-sure-m2
helper=alpine:3.23 # the catalog's alpine (internal/catalog/images.go)
cmd="${1:-}" dir="${2:-}"
if [ -z "$cmd" ] || [ -z "$dir" ]; then
	echo "usage: $0 load-images|save-images|load-maven|save-maven DIR [LIST]" >&2
	exit 2
fi
mkdir -p "$dir"
dir="$(cd "$dir" && pwd -P)"

case "$cmd" in
load-images)
	if [ -f "$dir/images.tar" ]; then
		docker load -i "$dir/images.tar" | tail -n 3
	else
		echo "no cached images"
	fi
	;;
save-images)
	list="${3:?save-images needs the image list}"
	images=()
	while IFS= read -r image; do images+=("$image"); done < <(sort -u "$list")
	[ "${#images[@]}" -gt 0 ] || {
		echo "no images to save" >&2
		exit 1
	}
	printf '%s\n' "${images[@]}"
	docker save -o "$dir/images.tar.tmp" "${images[@]}"
	mv "$dir/images.tar.tmp" "$dir/images.tar"
	du -h "$dir/images.tar"
	;;
load-maven)
	if [ -f "$dir/m2.tar" ]; then
		docker volume create "$volume" > /dev/null
		docker run --rm -v "$volume:/m2" -v "$dir:/cache:ro" "$helper" tar -C /m2 -xf /cache/m2.tar
		echo "restored $volume"
	else
		echo "no cached Maven repository"
	fi
	;;
save-maven)
	if ! docker volume inspect "$volume" > /dev/null 2>&1; then
		echo "no $volume volume: nothing was built"
		exit 0
	fi
	docker run --rm -v "$volume:/m2:ro" -v "$dir:/cache" "$helper" tar -C /m2 -cf /cache/m2.tar.tmp .
	mv "$dir/m2.tar.tmp" "$dir/m2.tar"
	du -h "$dir/m2.tar"
	;;
*)
	echo "unknown command $cmd" >&2
	exit 2
	;;
esac
