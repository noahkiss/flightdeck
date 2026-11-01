#!/bin/sh
# Notarize and staple the Release build that scripts/build.sh made.
# Needs a Developer ID build (Config/Local.xcconfig) and notarytool credentials
# stored once in the Keychain under a profile name:
#
#   xcrun notarytool store-credentials <profile> --key AuthKey.p8 --key-id <KEY_ID> --issuer <ISSUER_ID>
#   NOTARY_PROFILE=<profile> apple/scripts/notarize.sh
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
if [ -z "${NOTARY_PROFILE:-}" ]; then
  # Fill in the key ID and issuer from Config/Local.env when it exists.
  key_id='<KEY_ID>' issuer='<ISSUER_ID>'
  if [ -f "$here/Config/Local.env" ]; then
    . "$here/Config/Local.env"
    key_id=${ASC_KEY_ID:-$key_id}
    [ -n "${ASC_ISSUER_CMD:-}" ] && issuer=$(sh -c "$ASC_ISSUER_CMD" 2>/dev/null || echo "$issuer")
  fi
  cat >&2 <<MSG
notarize: set NOTARY_PROFILE to a notarytool keychain profile. Store one once with:

  xcrun notarytool store-credentials <profile> --key AuthKey.p8 --key-id $key_id --issuer $issuer

then run: NOTARY_PROFILE=<profile> apple/scripts/notarize.sh
MSG
  exit 1
fi
app=$(find "$here/build/Build/Products/Release" -maxdepth 1 -name '*.app' | head -1)
[ -n "$app" ] || { echo "no Release build; run scripts/build.sh first" >&2; exit 1; }
zip="$here/build/notarize.zip"
ditto -c -k --keepParent "$app" "$zip"
xcrun notarytool submit "$zip" --keychain-profile "$NOTARY_PROFILE" --wait
rm -f "$zip"
xcrun stapler staple "$app"
spctl -a -vv -t exec "$app"
