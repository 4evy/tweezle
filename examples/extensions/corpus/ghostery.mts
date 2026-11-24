import type { Node } from "@babel/types";
import { declarations, literal, parseSource, type Data } from "./literals.mts";

type Values = Record<string, any>;

function memberMatches(node: Node, object: string, property: string): boolean {
  return (
    node.type === "MemberExpression" &&
    !node.computed &&
    node.object.type === "Identifier" &&
    node.object.name === object &&
    node.property.type === "Identifier" &&
    node.property.name === property
  );
}

export function ghosteryConfiguration(read: (file: string) => string) {
  const files: string[] = [];
  const recordSchemas: Record<string, Data> = {};
  const defaultRegions = ["bg"];
  const globals: Record<string, Data> = {
    __CHROMIUM__: true,
    __FIREFOX__: false,
    DEFAULT_REGIONS: defaultRegions,
  };
  function normalize(value: Data, key: string): Data {
    if (Array.isArray(value))
      return value.map((member, index) => normalize(member, `${key}/${index}`));
    if (value !== null && typeof value === "object") {
      if (Object.hasOwn(value, "$record")) {
        recordSchemas[key] = normalize(value.$record, `${key}/*`);
        return {};
      }
      return Object.fromEntries(
        Object.entries(value).map(([name, member]) => [
          name,
          normalize(member, `${key}/${name}`),
        ]),
      );
    }
    return value;
  }
  const bindingsByFile = new Map<string, Map<string, Node>>();
  function declaration(file: string, name: string): Data {
    let bindings = bindingsByFile.get(file);
    if (!bindings) {
      files.push(file);
      bindings = declarations(parseSource(read(file)));
      bindingsByFile.set(file, bindings);
    }
    const node = bindings.get(name);
    if (!node) throw new Error(`${file}: missing ${name}`);
    // Interpret only the data parts of the Hybrids store declarations
    return normalize(
      literal(node, bindings, {
        skipProperty: (member) =>
          member.computed && memberMatches(member.key, "store", "connect"),
        resolve(node, value) {
          if (node.type === "Identifier" && Object.hasOwn(globals, node.name))
            return { value: globals[node.name] };
          if (memberMatches(node, "DEFAULT_REGIONS", "length"))
            return { value: defaultRegions.length };
          if (
            node.type === "ArrayExpression" &&
            node.elements.length === 1 &&
            node.elements[0]?.type === "Identifier" &&
            node.elements[0].name === "String"
          )
            return { value: [] };
          if (node.type === "CallExpression") {
            if (
              node.callee.type === "Identifier" &&
              node.callee.name === "isWebkit" &&
              node.arguments.length === 0
            )
              return { value: false };
            if (
              memberMatches(node.callee, "store", "record") &&
              node.arguments.length === 1
            )
              return { value: { $record: value(node.arguments[0]) } };
          }
          return undefined;
        },
      }),
      name,
    );
  }
  function dataObject(value: Data, context: string): Record<string, Data> {
    if (value === null || typeof value !== "object" || Array.isArray(value))
      throw new Error(`${context}: expected a data object`);
    return value;
  }
  function objectDeclaration(file: string, name: string): Values {
    return dataObject(declaration(file, name), `${file}: ${name}`);
  }
  function recordTemplate(name: string): Record<string, Data> {
    return dataObject(recordSchemas[name], name);
  }
  const options = objectDeclaration("src/store/options.js", "Options");
  const version = declaration("src/store/options.js", "OPTIONS_VERSION");
  const local: Values = {
    options,
    optionsVersion: version,
    customFilters: objectDeclaration(
      "src/store/custom-filters.js",
      "CustomFilters",
    ),
    elementPickerSelectors: objectDeclaration(
      "src/store/element-picker-selectors.js",
      "ElementPickerSelectors",
    ),
    disabledFilters: objectDeclaration(
      "src/store/disabled-filters.js",
      "DisabledFilters",
    ),
  };
  const defaults = structuredClone(local);
  options.regionalFilters = { enabled: true, regions: ["bg", "de"] };
  options.theme = "dark";
  options.panel.statsType = "list";
  options.customFilters.enabled = true;
  options.customFilters.filterLists["https://example.com/filters.txt"] = {
    ...recordTemplate("Options/customFilters/filterLists"),
    enabled: false,
  };
  options.redirectProtection.exceptions["example.com"] = true;
  options.exceptions["sample-tracker-id"] = {
    ...recordTemplate("Options/exceptions"),
    domains: ["example.com"],
  };
  options.paused["example.org"] = structuredClone(
    recordSchemas["Options/paused"],
  );
  options.zapped["example.net"] = false;
  const distractionsFile = "src/background/rule_resources/distractions.json";
  files.push(distractionsFile);
  const distractions = JSON.parse(read(distractionsFile)) as Record<
    string,
    unknown
  >;
  options.distractions = Object.fromEntries(
    Object.keys(distractions)
      .sort()
      .map((id) => [id, false]),
  );
  local.customFilters.text =
    "! Hypothetical sample filters\n||ads.example.com^\nexample.com##.sample-ad\n";
  local.customFilters.filterLists["https://example.com/filters.txt"] = {
    ...recordTemplate("CustomFilters/filterLists"),
    name: "Disabled sample list",
  };
  local.elementPickerSelectors.hostnames["example.com"] = [
    ".sample-banner",
    "#sample-popup",
  ];
  local.disabledFilters.ids["sample-filter-id"] = false;
  // Source record templates add paths that empty dictionary defaults cannot expose
  defaults.options = structuredClone(options);
  defaults.customFilters.filterLists = structuredClone(
    local.customFilters.filterLists,
  );
  defaults.elementPickerSelectors = structuredClone(
    local.elementPickerSelectors,
  );
  defaults.disabledFilters = structuredClone(local.disabledFilters);
  const localOnly = declaration("src/store/options.js", "LOCAL_OPTIONS");
  if (
    !Array.isArray(localOnly) ||
    !localOnly.every((key) => typeof key === "string")
  )
    throw new Error("Ghostery LOCAL_OPTIONS is not an array of strings");
  const sync = {
    options: Object.fromEntries(
      Object.entries(options).filter(
        ([key]) => key === "revision" || !localOnly.includes(key),
      ),
    ),
  };
  files.push(
    "src/background/sync.js",
    "src/utils/regions.js",
    "src/utils/storage.js",
    "src/store/managed-config.js",
    "src/managed_storage.json",
  );
  const managedSchema = JSON.parse(read("src/managed_storage.json")) as {
    properties: Record<string, unknown>;
  };
  const managed = {
    disableOnboarding: false,
    disableUserControl: false,
    disableUserAccount: true,
    disableTrackersPreview: false,
    trustedDomains: ["example.org"],
    customFilters: ["! Hypothetical managed filter", "||ads.example.org^"],
  };
  if (
    JSON.stringify(Object.keys(managed).sort()) !==
    JSON.stringify(Object.keys(managedSchema.properties).sort())
  )
    throw new Error("Ghostery managed policy coverage changed");
  return {
    local,
    sync,
    defaults,
    files: [...new Set(files)],
    recordSchemas,
    managed,
  };
}
