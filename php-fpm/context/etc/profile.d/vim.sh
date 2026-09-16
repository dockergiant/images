# shellcheck shell=bash
if [ -n "${BASH_VERSION-}" ] || [ -n "${KSH_VERSION-}" ] || [ -n "${ZSH_VERSION-}" ]; then
  [ "`/usr/bin/id -u 2>/dev/null || echo 0`" -le 200 ] && return
  # for bash and zsh, only if no alias is already set
  alias vi >/dev/null 2>&1 || alias vi=vim
fi
