// The dashboard uses vAPI.messaging to reach the background settings API
// https://github.com/gorhill/uBlock/blob/master/src/js/messaging.js
const send = async (what, args = {}) => {
  if (!globalThis.vAPI?.messaging?.send) {
    throw new Error("uBlock Origin dashboard messaging is unavailable");
  }
  return vAPI.messaging.send("dashboard", { what, ...args });
};
while (!(await send("readyToFilter"))) {
  await new Promise((resolve) => setTimeout(resolve, 100));
}
const currentUser = await send("userSettings");
const currentHidden = await send("readHiddenSettings");
const policy = (await chrome.storage.managed?.get(null)) ?? {};
const backup =
  typeof policy.adminSettings === "string"
    ? JSON.parse(policy.adminSettings || "{}")
    : (policy.adminSettings ?? {});
const overrides = policy.toOverwrite ?? {};
const managedUser = new Set([
  ...Object.keys(backup.userSettings ?? {}),
  ...(Array.isArray(policy.userSettings)
    ? policy.userSettings.map(([name]) => name)
    : []),
]);
if (
  settings.filter_lists !== undefined &&
  (overrides.filterLists?.length || Array.isArray(backup.selectedFilterLists))
) {
  throw new Error("uBlock Origin filter lists are managed by policy");
}
if (
  settings.trusted_sites !== undefined &&
  (overrides.trustedSiteDirectives?.length ||
    Array.isArray(backup.whitelist) ||
    typeof backup.netWhitelist === "string")
) {
  throw new Error("uBlock Origin trusted sites are managed by policy");
}
if (
  settings.rules !== undefined &&
  [
    backup.dynamicFilteringString,
    backup.urlFilteringString,
    backup.hostnameSwitchesString,
  ].some((value) => typeof value === "string")
) {
  throw new Error("uBlock Origin rules are managed by policy");
}
if (
  settings.user_filters !== undefined &&
  (Array.isArray(overrides.filters) || typeof backup.userFilters === "string")
) {
  throw new Error("uBlock Origin user filters are managed by policy");
}
for (const key of Object.keys(settings.user_settings ?? {})) {
  if (!Object.hasOwn(currentUser, key)) {
    throw new Error(`Unknown uBlock Origin user setting: ${key}`);
  }
  if (managedUser.has(key)) {
    throw new Error(`uBlock Origin user setting is managed by policy: ${key}`);
  }
  if (
    typeof settings.user_settings[key] !== typeof currentUser[key] ||
    Array.isArray(settings.user_settings[key]) !==
      Array.isArray(currentUser[key])
  ) {
    throw new Error(`Invalid type for uBlock Origin user setting: ${key}`);
  }
}
for (const key of Object.keys(settings.hidden_settings ?? {})) {
  if (!Object.hasOwn(currentHidden.default, key)) {
    throw new Error(`Unknown uBlock Origin hidden setting: ${key}`);
  }
  if (Object.hasOwn(currentHidden.admin ?? {}, key)) {
    throw new Error(
      `uBlock Origin hidden setting is managed by policy: ${key}`,
    );
  }
  if (
    typeof settings.hidden_settings[key] !== typeof currentHidden.default[key]
  ) {
    throw new Error(`Invalid type for uBlock Origin hidden setting: ${key}`);
  }
}
let hiddenContent;
if (settings.hidden_settings) {
  const values = { ...currentHidden.current, ...settings.hidden_settings };
  const lines = [];
  for (const [name, value] of Object.entries(values)) {
    if (Object.hasOwn(currentHidden.admin ?? {}, name)) continue;
    if (value === currentHidden.default[name]) continue;
    if (
      (typeof value === "string" &&
        (value === "" ||
          value !== value.trim() ||
          /[\r\n\u2028\u2029]/u.test(value))) ||
      (typeof value === "number" && !Number.isSafeInteger(value))
    ) {
      throw new Error(
        `uBlock Origin dashboard cannot represent hidden setting: ${name}`,
      );
    }
    lines.push(`${name} ${value}`);
  }
  hiddenContent = lines.join("\n");
}
for (const [name, value] of Object.entries(settings.user_settings ?? {})) {
  await send("userSettings", { name, value });
}
if (hiddenContent !== undefined) {
  await send("writeHiddenSettings", { content: hiddenContent });
  const { current } = await send("readHiddenSettings");
  for (const [name, value] of Object.entries(settings.hidden_settings)) {
    if (current[name] !== value) {
      throw new Error(`uBlock Origin rejected hidden setting: ${name}`);
    }
  }
}
if (settings.filter_lists) {
  const imported = settings.filter_lists.filter((key) =>
    /^https?:\/\//.test(key),
  );
  await send("applyFilterListSelection", {
    toSelect: settings.filter_lists,
    toImport: imported.join("\n"),
    merge: false,
  });
}
if (settings.trusted_sites) {
  await send("setWhitelist", { whitelist: settings.trusted_sites.join("\n") });
}
if (settings.rules !== undefined) {
  const current = await send("getRules");
  await send("modifyRuleset", {
    permanent: true,
    toRemove: current.permanentRules.join("\n"),
    toAdd: settings.rules,
  });
}
if (settings.user_filters !== undefined) {
  const current = await send("readUserFilters");
  const result = await send("writeUserFilters", {
    content: settings.user_filters,
    enabled: settings.filter_lists
      ? settings.filter_lists.includes("user-filters")
      : current.enabled,
    trusted: current.trusted,
  });
  if (result?.error) throw new Error(result.error);
}
await send("reloadAllFilters");
// Read storage after the settings API calls so asynchronous writes are flushed
await chrome.storage.local.get(null);
