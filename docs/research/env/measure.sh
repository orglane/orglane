#!/bin/sh
# Memory and image footprint of one Docker Compose project, for the provider
# research reports in docs/research/.
#
# Usage:
#   measure.sh mem <project> [samples] [interval-seconds]
#       Sums the memory of all running containers of the project (as shown by
#       `docker stats`, which excludes the page cache) and prints one line per
#       sample, then the min / median / max of the sums and a per-container
#       breakdown of the last sample. All values are MiB.
#   measure.sh peak <project> <seconds>
#       Samples every 2 seconds for <seconds> and prints only the maximum sum
#       and the per-container breakdown at that maximum. Run it in the
#       background while generating load.
#   measure.sh images <project>
#       Lists images used by the project's containers and sums Docker .Size
#       (uncompressed bytes, not deduplicated on-disk usage).
set -eu

usage() {
	echo "usage: $0 mem <project> [samples] [interval] | peak <project> <seconds> | images <project>" >&2
	exit 2
}

[ $# -ge 2 ] || usage
cmd=$1
project=$2

containers() {
	docker ps -q --filter "label=com.docker.compose.project=$project"
}

# sample prints "<name> <MiB>" per running container of the project.
sample() {
	ids=$(containers)
	[ -n "$ids" ] || { echo "no running containers for project $project" >&2; exit 1; }
	# shellcheck disable=SC2086 # word splitting of the id list is intended
	docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' $ids |
		awk '{
			v = $2; u = v; sub(/[0-9.]+/, "", u); sub(/[A-Za-z]+$/, "", v)
			f = 1
			if (u == "B") f = 1 / 1048576
			else if (u == "KiB" || u == "kB") f = 1 / 1024
			else if (u == "MiB" || u == "MB") f = 1
			else if (u == "GiB" || u == "GB") f = 1024
			printf "%s %.1f\n", $1, v * f
		}'
}

total() {
	awk '{ s += $2 } END { printf "%.1f\n", s }'
}

case $cmd in
mem)
	samples=${3:-5}
	interval=${4:-10}
	sums=""
	i=1
	while :; do
		last=$(sample)
		sum=$(printf '%s\n' "$last" | total)
		echo "sample $i: $sum MiB"
		sums="$sums $sum"
		[ "$i" -ge "$samples" ] && break
		i=$((i + 1))
		sleep "$interval"
	done
	# shellcheck disable=SC2086 # the space-separated numeric samples become lines
	printf '%s\n' $sums | sort -n | awk '{ a[NR] = $1 } END {
		m = (NR % 2) ? a[(NR + 1) / 2] : (a[NR / 2] + a[NR / 2 + 1]) / 2
		printf "min %.1f  median %.1f  max %.1f MiB (%d samples)\n", a[1], m, a[NR], NR
	}'
	echo "last sample by container:"
	printf '%s\n' "$last" | sort -k2 -rn | awk '{ printf "  %-50s %8.1f MiB\n", $1, $2 }'
	;;
peak)
	[ $# -ge 3 ] || usage
	end=$(($(date +%s) + $3))
	max=0
	best=""
	while [ "$(date +%s)" -lt "$end" ]; do
		cur=$(sample)
		sum=$(printf '%s\n' "$cur" | total)
		if awk -v a="$sum" -v b="$max" 'BEGIN { exit !(a > b) }'; then
			max=$sum
			best=$cur
		fi
		sleep 2
	done
	echo "peak $max MiB"
	printf '%s\n' "$best" | sort -k2 -rn | awk '{ printf "  %-50s %8.1f MiB\n", $1, $2 }'
	;;
images)
	docker ps -a --filter "label=com.docker.compose.project=$project" --format '{{.Image}}' | sort -u |
		while read -r image; do
			printf '%s %s\n' "$image" "$(docker image inspect -f '{{.Size}}' "$image")"
		done |
		awk '{ s += $2; n++; printf "  %-70s %8.1f MB\n", $1, $2 / 1e6 } END { printf "%d images, %.1f MB total\n", n, s / 1e6 }'
	;;
*)
	usage
	;;
esac
