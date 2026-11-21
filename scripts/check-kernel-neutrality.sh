#!/usr/bin/env bash
# Kernel neutrality gate. The kernel is general: no niche vocabulary and no
# Spanish prose in non-test Go code (user-facing text goes through i18n keys /
# stable error codes; fallback messages are English).
#
# Windows: .gitattributes pins *.sh and scripts/*.txt to LF; run from Git Bash (perl required).
# Usage: scripts/check-kernel-neutrality.sh [base-ref]
#   --update   rewrite the counts in scripts/kernel-neutrality-baseline.txt
#              (only allowed to LOWER them; use after you removed offenders).
#
# Two checks over non-test, non-comment code (comment-only lines and trailing
# // comments are ignored):
#   niche   : CFDI, SAT, MXN, llanta, vehicle plate, folio fiscal, ClaveProd*,
#             es-MX, and RFC used as a tax id (RFC 3339 / RFC 5322 etc. are fine)
#   spanish : any of áéíóúñ¿¡ in code, or common unaccented Spanish error phrases
#             inside a string literal (debe ser, falta, no se pudo, no existe...)
#
# scripts/kernel-neutrality-baseline.txt lists "<path> <check> <count>" with a
# "# justification" line above each group. A file/check not listed must have 0
# hits; a listed one may not exceed its count, and a count above its value on
# base-ref (default origin/main) is refused: the baseline only shrinks. A
# listed count that is higher than the real one fails too (tighten it).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
BASELINE=scripts/kernel-neutrality-baseline.txt
update=0; base=""
for a in "$@"; do case "$a" in --update) update=1;; *) base="$a";; esac; done
if [ -z "$base" ] && git rev-parse --verify -q origin/main >/dev/null; then base=origin/main; fi

scan() {
  git ls-files '*.go' ':!:*_test.go' ':!:scripts/*' ':!:**/testdata/**' | while read -r f; do
    [ -f "$f" ] || continue
    perl -Mutf8 -CSD -ne '
      next if /^\s*(\/\/|\*|\/\*)/;
      while (/\s\/\//g) { my $pre = substr($_, 0, $-[0]); my $q = () = $pre =~ /"/g; my $b = () = $pre =~ /`/g; if ($q % 2 == 0 && $b % 2 == 0) { $_ = $pre; last } }
      $n{niche}++   if /\b(CFDI|SAT|MXN|ClaveProd\w*)\b|\bRFC\b(?![\s-]*\d)(?!-PAYMENTS)|(?i:\b(llantas?|folio fiscal|vehicle[ _]plate)\b|es-MX|"rfc")/;
      $n{spanish}++ if /[áéíóúñ¿¡]/ || /"[^"]*\b(debe ser|falta |no se pudo|no existe|desactivad[ao]s?|automatizaci[oó]n)\b/;
      END { printf "%s %s %d\n", $ARGV, $_, $n{$_} for grep { $n{$_} } sort keys %n }
    ' "$f" | sed "s|^[^ ]*|$f|"
  done
}
current=$(scan)

if [ "$update" = 1 ]; then
  tmp=$(mktemp)
  # keep justification comments; rewrite the counts, drop entries now at 0
  while IFS= read -r line; do
    case "$line" in ''|\#*) echo "$line";; *)
      read -r f k _ <<<"$line"
      c=$(awk -v f="$f" -v k="$k" '$1==f&&$2==k{print $3}' <<<"$current")
      [ -n "$c" ] && echo "$f $k $c";;
    esac
  done <"$BASELINE" >"$tmp"
  mv "$tmp" "$BASELINE"; echo "baseline updated"; exit 0
fi

fail=0
while read -r f k c; do
  [ -z "${f:-}" ] && continue
  b=$(awk -v f="$f" -v k="$k" '$1==f&&$2==k{print $3}' "$BASELINE" | head -1)
  b=${b:-0}
  if [ "$c" -gt "$b" ]; then echo "FAIL $f: $k hits $c > allowed $b" >&2; fail=1; fi
done <<<"$current"
while read -r f k b; do
  case "$f" in ''|\#*) continue;; esac
  c=$(awk -v f="$f" -v k="$k" '$1==f&&$2==k{print $3}' <<<"$current" | head -1); c=${c:-0}
  if [ "$c" -lt "$b" ]; then echo "FAIL $f: $k now $c < baseline $b; lower it (scripts/check-kernel-neutrality.sh --update)" >&2; fail=1; fi
  if [ -n "$base" ] && git cat-file -e "$base:$BASELINE" 2>/dev/null; then
    ob=$(git show "$base:$BASELINE" | awk -v f="$f" -v k="$k" '$1==f&&$2==k{print $3}' | head -1)
    if [ -n "$ob" ] && [ "$b" -gt "$ob" ]; then echo "FAIL $f: baseline raised $ob -> $b" >&2; fail=1; fi
    if [ -z "$ob" ] && git show "$base:$BASELINE" >/dev/null 2>&1; then echo "FAIL $f $k: new baseline entry (baseline only shrinks)" >&2; fail=1; fi
  fi
done <"$BASELINE"
[ "$fail" = 0 ] && echo "kernel neutrality gate: ok"
exit "$fail"
