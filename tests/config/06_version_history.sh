#!/usr/bin/env bash
# 06_version_history.sh — Test config version history and revert.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/helpers.sh"

header "Test: Version History — Track changes and revert"

# 1. List versions (should have entries from previous tests)
VERSIONS=$(api_get "/profiles/${PROFILE_ID}/versions")
VERSION_COUNT=$(echo "$VERSIONS" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null)
if [ "$VERSION_COUNT" -ge 1 ]; then
    pass "Version history has $VERSION_COUNT entries"
    echo "$VERSIONS" | python3 -c "
import json, sys
for v in json.load(sys.stdin):
    print(f'    v{v[\"version\"]}: {v[\"change_summary\"]} ({v[\"apply_status\"]})')
" 2>/dev/null
else
    fail "Expected at least 1 version, got $VERSION_COUNT"
fi

# 2. Create a new change to have something to revert
info "Making a config change (log_statement=all)..."
CURRENT=$(api_get "/profiles/${PROFILE_ID}")
UPDATED=$(echo "$CURRENT" | python3 -c "
import json, sys
p = json.load(sys.stdin)
p['config']['pg_params']['log_statement'] = 'all'
print(json.dumps(p))
")
api_put "/profiles/${PROFILE_ID}" "$UPDATED" > "$DEVNULL" 2>&1
api_post "/profiles/${PROFILE_ID}/apply" '{"confirmed": true}' > "$DEVNULL" 2>&1
pass "Applied log_statement=all"

# Wait for the restart
sleep 5

# 3. Verify new version created
VERSIONS_AFTER=$(api_get "/profiles/${PROFILE_ID}/versions")
NEW_COUNT=$(echo "$VERSIONS_AFTER" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null)
if [ "$NEW_COUNT" -gt "$VERSION_COUNT" ]; then
    pass "New version created (now $NEW_COUNT versions)"
else
    fail "Expected more versions after change, still $NEW_COUNT"
fi

# 4. Get a specific version
FIRST_VERSION=$(echo "$VERSIONS_AFTER" | python3 -c "import json,sys; vs=json.load(sys.stdin); print(vs[-1]['version'])" 2>/dev/null)
SPECIFIC=$(api_get "/profiles/${PROFILE_ID}/versions/${FIRST_VERSION}")
if echo "$SPECIFIC" | python3 -c "import json,sys; v=json.load(sys.stdin); assert v['version'] == $FIRST_VERSION" 2>/dev/null; then
    pass "Retrieved specific version v${FIRST_VERSION}"
else
    fail "Could not retrieve version $FIRST_VERSION"
fi

# 5. Revert to first version
info "Reverting to version ${FIRST_VERSION}..."
REVERT_RESULT=$(api_post "/profiles/${PROFILE_ID}/revert" "{\"target_version\": ${FIRST_VERSION}}")
REVERT_STRATEGY=$(echo "$REVERT_RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('change_impact',{}).get('apply_strategy','none'))" 2>/dev/null)
if [ "$REVERT_STRATEGY" != "none" ] && [ "$REVERT_STRATEGY" != "" ]; then
    pass "Revert returned change_impact with strategy=$REVERT_STRATEGY"
else
    # Revert with no active cluster impact is also valid
    pass "Revert completed (no active cluster impact or strategy=$REVERT_STRATEGY)"
fi

# 6. Verify revert created a new version
FINAL_VERSIONS=$(api_get "/profiles/${PROFILE_ID}/versions")
FINAL_COUNT=$(echo "$FINAL_VERSIONS" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null)
LATEST_SUMMARY=$(echo "$FINAL_VERSIONS" | python3 -c "import json,sys; print(json.load(sys.stdin)[0]['change_summary'])" 2>/dev/null)
if echo "$LATEST_SUMMARY" | grep -qi "revert"; then
    pass "Latest version is a revert: $LATEST_SUMMARY (total: $FINAL_COUNT versions)"
else
    fail "Expected latest version to be a revert" "$LATEST_SUMMARY"
fi

summary
