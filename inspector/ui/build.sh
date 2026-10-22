#!/bin/sh
# Builds ui/dist from ui/src: copies the assets, splices views/<name>.html into
# index.html at each <!-- view:<name> --> marker, and compiles app.css.
# Usage: ui/build.sh <tailwindcss binary>
set -eu

tailwind=${1:?usage: ui/build.sh <tailwindcss binary>}
case "$tailwind" in
  /*) ;;
  *) tailwind="$(pwd)/$tailwind" ;;
esac
cd "$(dirname "$0")"

fail() {
  echo "ui/build.sh: $*" >&2
  exit 1
}

[ -f src/styles.css ] || fail "src/styles.css missing"
[ -f src/index.html ] || fail "src/index.html missing"

# The page runs under a CSP without unsafe-inline and Alpine's CSP build, so
# these would either be blocked in the browser or put panel content into the DOM
# as markup.
bad=$(grep -rnE 'x-html|innerHTML|outerHTML|insertAdjacentHTML|document\.write' src || true)
[ -z "$bad" ] || fail "markup injection is not allowed:
$bad"
bad=$(find src -name '*.html' -exec grep -HnE '<[^>]*[[:space:]](style|on[a-z]+)=' {} + || true)
[ -z "$bad" ] || fail "inline style or event attributes are blocked by the CSP:
$bad"
bad=$(find src -name '*.html' -exec grep -HnE '<script' {} + | grep -v 'src=' || true)
[ -z "$bad" ] || fail "inline scripts are blocked by the CSP:
$bad"

mkdir -p dist
find dist -mindepth 1 ! -name .keep -exec rm -rf {} +
cp -R src/. dist/
rm -rf dist/views dist/styles dist/styles.css

awk '
  match($0, /<!-- view:[a-z0-9-]+ -->/) {
    name = substr($0, RSTART + 10, RLENGTH - 14)
    file = "src/views/" name ".html"
    printf "%s", substr($0, 1, RSTART - 1)
    n = 0
    while ((r = (getline line < file)) > 0) { print line; n++ }
    if (r < 0 || n == 0) { print "ui/build.sh: missing or empty " file > "/dev/stderr"; exit 2 }
    close(file)
    rest = substr($0, RSTART + RLENGTH)
    if (rest != "") print rest
    next
  }
  { print }
' src/index.html > dist/index.html || fail "assembling index.html failed"
if grep -q '<!-- view:' dist/index.html; then
  fail "a view marker survived assembly"
fi

for f in src/views/*.html; do
  name=$(basename "$f" .html)
  grep -q "<!-- view:$name -->" src/index.html || fail "src/views/$name.html has no <!-- view:$name --> marker in index.html"
done

mkdir -p dist/vendor
cp -R vendor/. dist/vendor/

cd src
"$tailwind" -i styles.css -o ../dist/app.css --minify
