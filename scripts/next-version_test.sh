#!/bin/sh
# Run: sh scripts/next-version_test.sh
cd "$(dirname "$0")" || exit 1
fail=0
check() { # want bump current
  got=$(sh ./next-version.sh "$2" "$3" 2>/dev/null)
  if [ "$got" != "$1" ]; then echo "FAIL: $2 $3 -> '$got', want '$1'"; fail=1; fi
}
check v1.2.4  patch v1.2.3
check v1.3.0  minor v1.2.3
check v2.0.0  major v1.2.3
check v0.0.1  patch v0.0.0
check v1.10.0 minor v1.9.9
check v1.0.0  major v0.9.12
for bad in "patch 1.2.3" "patch v1.2" "patch v1.2.3-rc1" "huge v1.2.3" ""; do
  # shellcheck disable=SC2086
  if sh ./next-version.sh $bad >/dev/null 2>&1; then echo "FAIL: accepted '$bad'"; fail=1; fi
done
[ $fail -eq 0 ] && echo ok
exit $fail
