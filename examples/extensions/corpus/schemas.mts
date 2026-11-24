import { parse } from "@babel/parser";

import type {
  TSInterfaceDeclaration,
  TSTypeAliasDeclaration,
  TSTypeElement,
} from "@babel/types";
import { propertyName } from "./literals.mts";

export function schemaFields(members: readonly TSTypeElement[]): string[] {
  const fields: string[] = [];
  for (const member of members) {
    if (
      member.type !== "TSPropertySignature" &&
      member.type !== "TSMethodSignature"
    )
      continue;
    const key = member.computed ? undefined : propertyName(member.key);
    if (key !== undefined) fields.push(key);
  }
  return fields;
}

// Compare sample objects with interface fields, including optional properties
export function checkSchemas(
  source: string,
  examples: Record<string, object[]>,
) {
  const ast = parse(source, { sourceType: "module", plugins: ["typescript"] });
  const declarations = new Map<
    string,
    TSInterfaceDeclaration | TSTypeAliasDeclaration
  >();
  for (const statement of ast.program.body) {
    const node =
      statement.type === "ExportNamedDeclaration"
        ? statement.declaration
        : statement;
    if (
      node?.type === "TSInterfaceDeclaration" ||
      node?.type === "TSTypeAliasDeclaration"
    )
      declarations.set(node.id.name, node);
  }
  const result: Record<
    string,
    { declared_fields: string[]; configured_fields: string[] }
  > = {};
  for (const [name, values] of Object.entries(examples)) {
    const declaration = declarations.get(name);
    const members =
      declaration?.type === "TSInterfaceDeclaration"
        ? declaration.body.body
        : declaration?.typeAnnotation.type === "TSTypeLiteral"
          ? declaration.typeAnnotation.members
          : undefined;
    if (!members) throw new Error(`Missing object schema ${name}`);
    const fields = schemaFields(members).sort();
    const configured = [
      ...new Set(values.flatMap((value) => Object.keys(value))),
    ].sort();
    const missing = fields.filter((key) => !configured.includes(key));
    if (missing.length)
      throw new Error(
        `${name}: missing optional/required fields ${missing.join(", ")}`,
      );
    result[name] = { declared_fields: fields, configured_fields: configured };
  }
  return result;
}
