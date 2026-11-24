import path from "node:path";
import type { ClassDeclaration, Node, ObjectExpression } from "@babel/types";
import {
  declarations,
  literal,
  parseSource,
  property,
  visit,
  propertyName,
  type Data,
} from "./literals.mts";

type Values = Record<string, any>;

function dataString(value: Data): string {
  if (typeof value !== "string") throw new Error("Expected a string");
  return value;
}

function classField(definition: ClassDeclaration, name: string) {
  for (const field of definition.body.body) {
    if (
      field.type === "ClassProperty" &&
      field.static &&
      !field.computed &&
      propertyName(field.key) === name
    )
      return field.value;
  }
  return undefined;
}

export function resConfiguration(read: (file: string) => string) {
  const files = ["lib/modules/index.js", "lib/modules/hosts/index.js"];
  const parseFile = (file: string) => {
    if (!files.includes(file)) files.push(file);
    return parseSource(read(file));
  };
  const local: Values = { "RES.modulePrefs": {} };
  const tables: { module: string; option: string; fields: string[] }[] = [];
  const skipped: string[] = [];
  const descriptors = new Map<
    string,
    { node: Node; bindings: ReadonlyMap<string, Node> }
  >();
  let optionCount = 0;
  let hostCount = 0;
  function options(
    module: string,
    node: ObjectExpression,
    bindings: ReadonlyMap<string, Node>,
  ) {
    const values = (local[`RESoptions.${module}`] ??= {});
    for (const member of node.properties) {
      if (member.type === "SpreadElement" && module === "showImages") continue;
      if (member.type !== "ObjectProperty" || member.computed)
        throw new Error(`${module}: unexpected options member`);
      const name = propertyName(member.key);
      if (name === undefined)
        throw new Error(`${module}: unexpected option name`);
      const value = property(member.value, "value");
      if (!value) {
        const type = property(member.value, "type");
        if (
          (!type || literal(type, bindings) !== "button") &&
          `${module}.${name}` !== "presets.resetToFactory"
        )
          throw new Error(
            `${module}.${name}: unexplained nonpersistent option`,
          );
        skipped.push(`${module}.${name}`);
        continue;
      }
      try {
        values[name] = { value: literal(value, bindings) };
      } catch (error) {
        throw new Error(`${module}.${name}: ${error}`);
      }
      optionCount++;
      descriptors.set(`${module}.${name}`, { node: member.value, bindings });
      const fields = property(member.value, "fields");
      if (fields?.type === "ArrayExpression") {
        tables.push({
          module,
          option: name,
          fields: fields.elements.map((field) =>
            dataString(literal(property(field, "key"), bindings)),
          ),
        });
      } else if (fields)
        throw new Error(`${module}.${name}: invalid table fields`);
    }
  }
  const registry = parseFile("lib/modules/index.js");
  for (const entry of registry.program.body) {
    if (entry.type !== "ImportDeclaration") continue;
    const file = `lib/modules/${entry.source.value.slice(2)}.js`;
    const ast = parseFile(file);
    const bindings = declarations(ast);
    const module = bindings.get("module");
    if (
      module?.type !== "NewExpression" ||
      module.callee.type !== "Identifier" ||
      module.callee.name !== "Module"
    )
      throw new Error(`${file}: missing Module declaration`);
    const id = dataString(literal(module.arguments[0], bindings));
    let disabled = false;
    let always = false;
    visit(ast, (node) => {
      if (
        node.type !== "AssignmentExpression" ||
        node.left.type !== "MemberExpression" ||
        node.left.computed ||
        node.left.object.type !== "Identifier" ||
        node.left.object.name !== "module"
      )
        return;
      const name = propertyName(node.left.property);
      if (name === "disabledByDefault")
        disabled = Boolean(literal(node.right, bindings));
      if (name === "alwaysEnabled")
        always = Boolean(literal(node.right, bindings));
      if (name === "options") {
        if (node.right.type !== "ObjectExpression")
          throw new Error(`${id}: expected options object`);
        options(id, node.right, bindings);
      }
    });
    local["RES.modulePrefs"][id] = always || !disabled;
  }
  const hosts = parseFile("lib/modules/hosts/index.js");
  for (const entry of hosts.program.body) {
    if (entry.type !== "ImportDeclaration") continue;
    const ast = parseFile(
      `lib/modules/hosts/${entry.source.value.slice(2)}.js`,
    );
    const bindings = declarations(ast);
    const host = ast.program.body.find(
      (node) => node.type === "ExportDefaultDeclaration",
    )?.declaration;
    if (
      host?.type !== "NewExpression" ||
      host.callee.type !== "Identifier" ||
      host.callee.name !== "Host"
    )
      throw new Error(`Invalid host ${entry.source.value}`);
    const id = dataString(literal(host.arguments[0], bindings));
    if (["defaultImage", "defaultVideo", "defaultAudio"].includes(id)) continue;
    local["RESoptions.showImages"][`display_${id}`] = { value: true };
    optionCount++;
    hostCount++;
    const extra = property(host.arguments[1], "options");
    if (extra) {
      if (extra.type !== "ObjectExpression")
        throw new Error(`${id}: expected host options object`);
      options("showImages", extra, bindings);
    }
  }
  const defaults = structuredClone(local);
  // Hypothetical values exercise every positional table field without credentials
  const rows: Record<string, any[]> = {
    "accountSwitcher.accounts": ["sample-user", "", false],
    "commentDepth.subredditCommentDepths": ["testing", "4", "50"],
    "commentTools.macros": [
      "Sample macro",
      "Line one\nLine two: café",
      "sample",
      [77, true, false, false, false],
    ],
    "customToggles.toggle": ["sample-toggle", false, "Sample toggle"],
    "filteReddit.allowNSFW": ["testing", "everywhere"],
    "filteReddit.keywords": [
      "sample phrase",
      "include",
      "testing",
      "exception",
    ],
    "filteReddit.subreddits": ["sample_subreddit"],
    "filteReddit.comments": ["sample phrase", "include", "testing"],
    "filteReddit.users": ["sample-user"],
    "filteReddit.domains": ["example.com", "include", "testing"],
    "filteReddit.flair": ["sample flair", "include", "testing"],
    "hover.instances": ["sample-hover", false],
    "messageMenu.links": ["Sample", "https://example.com/"],
    "multiredditNavbar.sectionLinks": ["Sample", "https://example.com/"],
    "notifications.notificationTypes": ["sample-notification", false, false],
    "penaltyBox.features": ["showImages", "showViewImagesTab", false, "0"],
    "profileNavigator.sectionLinks": ["Sample", "https://example.com/"],
    "stylesheet.loadStylesheets": [
      "https://example.com/sample.css",
      "include",
      "testing",
      "sample-toggle",
    ],
    "stylesheet.snippets": [
      "body { --sample-color: #663399; }\n",
      "include",
      "testing",
      "sample-toggle",
    ],
    "stylesheet.bodyClasses": [
      "sample-class",
      "include",
      "testing",
      "sample-toggle",
    ],
    "subRedditTagger.subReddits": ["testing", "", "Sample tag"],
    "userTagger.presetTags": ["Sample tag", "none"],
    "usernameHider.perAccountDisplayText": [
      "sample-user",
      "Sample display name",
    ],
    "voteEnhancements.userDefinedLinkColoration": ["100", "#663399"],
    "voteEnhancements.userDefinedCommentColoration": ["100", "#663399"],
  };
  for (const table of tables) {
    const key = `${table.module}.${table.option}`;
    const row = rows[key];
    if (!row || row.length !== table.fields.length)
      throw new Error(`Incomplete table sample ${key}`);
    const { node, bindings } = descriptors.get(key)!;
    for (let index = 0; index < row.length; index++) {
      const fields = property(node, "fields");
      if (fields?.type !== "ArrayExpression")
        throw new Error(`${key}: invalid table fields`);
      const allowed = property(fields.elements[index], "values");
      if (allowed?.type === "ArrayExpression") {
        const items = literal(allowed, bindings);
        if (
          !Array.isArray(items) ||
          !items.some(
            (item) =>
              item !== null &&
              typeof item === "object" &&
              !Array.isArray(item) &&
              item.value === row[index],
          )
        )
          throw new Error(`${key}: invalid ${table.fields[index]} value`);
      }
    }
    local[`RESoptions.${table.module}`][table.option].value.push(row);
    defaults[`RESoptions.${table.module}`][table.option].value.push(row);
  }
  const cases = "lib/modules/filteReddit/cases.js";
  parseFile(cases);
  const filterCases: Record<string, Values[]> = {};
  for (const context of ["post", "comment", "browse"]) {
    const base = `lib/modules/filteReddit/${context}Cases`;
    const registry = parseFile(`${base}/index.js`);
    filterCases[context] = [];
    for (const entry of registry.program.body) {
      if (entry.type !== "ExportNamedDeclaration" || !entry.source) continue;
      const ast = parseFile(
        path.posix.normalize(`${base}/${entry.source.value}.js`),
      );
      const bindings = declarations(ast);
      for (const specifier of entry.specifiers) {
        if (specifier.type !== "ExportSpecifier")
          throw new Error(`${base}: unsupported ${specifier.type}`);
        const type = propertyName(specifier.exported);
        const localName = propertyName(specifier.local);
        const definition = ast.program.body
          .filter((node) => node.type === "ExportNamedDeclaration")
          .map((node) => node.declaration)
          .find(
            (node) =>
              node?.type === "ClassDeclaration" && node.id?.name === localName,
          );
        if (type === undefined || definition?.type !== "ClassDeclaration")
          throw new Error(`Missing filter case ${type}`);
        const defaults = classField(definition, "defaultConditions");
        let conditions: Values = {};
        if (type === "postAfter")
          conditions = { patt: "2026-01-01T00:00:00.000Z" };
        else if (type === "currentLocation")
          conditions = { patt: "/r/testing" };
        else if (type === "toggle")
          conditions = { toggleName: "sample-toggle" };
        else if (defaults) {
          const values = literal(defaults, bindings);
          if (
            values === null ||
            typeof values !== "object" ||
            Array.isArray(values)
          )
            throw new Error(`${type}: invalid default conditions`);
          conditions = values;
        } else if (
          definition.superClass?.type === "Identifier" &&
          definition.superClass.name === "PatternCase"
        )
          conditions = { patt: "sample", fullMatch: false };
        if (Object.hasOwn(conditions, "patt") && conditions.patt === "")
          conditions.patt = "sample";
        if (type === "currentSub") conditions.patt = "testing";
        if (type === "currentMulti")
          conditions = { user: "sample-user", name: "sample-multi" };
        if (type === "loggedInAs") conditions.loggedInAs = "sample-user";
        if (type === "dow") conditions.days = ["Mon", "Fri"];
        if (type === "hasExpando") conditions.types = ["image", "video"];
        const fields = classField(definition, "fields");
        visit(fields, (node) => {
          if (
            node.type !== "ObjectProperty" ||
            node.computed ||
            propertyName(node.key) !== "id"
          )
            return;
          const key = dataString(literal(node.value, bindings));
          if (!Object.hasOwn(conditions, key))
            throw new Error(`${type}: missing condition field ${key}`);
        });
        filterCases[context].push({ type, ...conditions });
      }
    }
  }
  for (const suffix of ["P", "C"]) {
    local["RESoptions.filteReddit"][`customFilters${suffix}`].value = [
      {
        note: "Hypothetical nested filter",
        ver: 3,
        body: {
          type: "group",
          op: "all",
          of: [
            {
              type: "group",
              op: "any",
              of: [
                ...filterCases[suffix === "P" ? "post" : "comment"],
                ...filterCases.browse,
              ],
            },
          ],
        },
        id: `customFilter-sample-${suffix}`,
        opts: {
          ondemand: true,
          name: `Sample ${suffix}`,
          ...(suffix === "C" ? { propagate: false } : {}),
        },
      },
    ];
    defaults["RESoptions.filteReddit"][`customFilters${suffix}`] =
      structuredClone(
        local["RESoptions.filteReddit"][`customFilters${suffix}`],
      );
  }
  files.push(
    "lib/core/options/storage.js",
    "lib/core/modules/storage.js",
    "lib/core/options/table.js",
    "lib/modules/filteReddit/browseCases/CurrentSub.js",
    "lib/modules/filteReddit/Case.js",
  );
  return {
    local,
    defaults,
    files,
    tables,
    skipped,
    optionCount,
    hostCount,
    filterCases,
  };
}
