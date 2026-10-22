#!/bin/sh
for f in /proc/net/tcp /proc/net/tcp6; do
  [ -r "$f" ] || continue
  while IFS= read -r l; do
    printf 'T %s\n' "$l"
  done < "$f"
done
self=$$
for d in /proc/[0-9]*; do
  pid=${d#/proc/}
  [ "$pid" = "$self" ] && continue
  # argv is the process's own choice; a newline in it would start a forged record
  cmd=$(tr '\0\n\r' '   ' < "$d/cmdline" 2>/dev/null) || continue
  [ -n "$cmd" ] || continue
  printf 'P %s %s\n' "$pid" "$cmd"
  # fd names are numeric, and one ls per process is cheaper than one readlink per fd
  # shellcheck disable=SC2012
  ls -l "$d/fd" 2>/dev/null | while IFS= read -r l; do
    case $l in
      *'-> socket:['*)
        i=${l##*socket:[}
        printf 'F %s %s\n' "$pid" "${i%]}"
        ;;
    esac
  done
done
exit 0
