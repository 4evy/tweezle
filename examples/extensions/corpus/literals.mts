import { parse } from "@babel/parser";

import type { File, Node, ObjectProperty } from "@babel/types";

type Scalar = string | number | boolean | null | undefined;
export type Data = Scalar | Data[] | { [key: string]: Data };

export function parseSource(source: string): File {
  return parse(source, { sourceType: "module", plugins: ["flow", "jsx"] });
}

function isNode(value: unknown): value is Node {
  return (
    value !== null &&
    typeof value === "object" &&
    "type" in value &&
    typeof value.type === "string"
  );
}

export function visit(
  node: Node | null | undefined,
  callback: (node: Node) => void,
): void {
  if (!node) return;
  callback(node);
  for (const [key, value] of Object.entries(node)) {
    if (
      [
        "loc",
        "comments",
        "leadingComments",
        "innerComments",
        "trailingComments",
        "tokens",
      ].includes(key)
    )
      continue;
    if (Array.isArray(value)) {
      for (const child of value) {
        if (isNode(child)) visit(child, callback);
      }
    } else if (isNode(value)) visit(value, callback);
  }
}

export function propertyName(node: Node): string | undefined {
  switch (node.type) {
    case "Identifier":
      return node.name;
    case "StringLiteral":
    case "NumericLiteral":
      return String(node.value);
    default:
      return undefined;
  }
}

export function property(
  node: Node | null | undefined,
  name: string,
): Node | undefined {
  if (node?.type !== "ObjectExpression") return undefined;
  return node.properties.find(
    (member): member is ObjectProperty =>
      member.type === "ObjectProperty" &&
      !member.computed &&
      propertyName(member.key) === name,
  )?.value;
}

type Dialect = {
  skipProperty?: (member: ObjectProperty) => boolean;
  resolve?: (
    node: Node,
    read: (node: Node) => Data,
  ) => { value: Data } | undefined;
};

function scalar(value: Data): Scalar {
  if (value === null || typeof value !== "object") return value;
  throw new Error("Expected a scalar data value");
}

// Interpret literals and explicitly supported data forms rather than running code
export function literal(
  node: Node | null | undefined,
  bindings: ReadonlyMap<string, Node>,
  dialect: Dialect = {},
): Data {
  const resolving = new Set<string>();
  function read(node: Node | null | undefined): Data {
    if (!node) throw new Error("Missing data expression");
    const extension = dialect.resolve?.(node, read);
    if (extension) return extension.value;
    switch (node.type) {
      case "StringLiteral":
      case "NumericLiteral":
      case "BooleanLiteral":
        return node.value;
      case "NullLiteral":
        return null;
      case "TypeCastExpression":
        return read(node.expression);
      case "ArrayExpression":
        return node.elements.map((member) => (member ? read(member) : null));
      case "ObjectExpression": {
        const result: Record<string, Data> = Object.create(null);
        for (const member of node.properties) {
          if (member.type === "SpreadElement") {
            Object.assign(result, read(member.argument));
          } else if (member.type === "ObjectProperty") {
            if (dialect.skipProperty?.(member)) continue;
            const key = member.computed
              ? String(scalar(read(member.key)))
              : propertyName(member.key);
            if (key === undefined)
              throw new Error(`Unsupported data key ${member.key.type}`);
            result[key] = read(member.value);
          } else throw new Error(`Non-data object member ${member.type}`);
        }
        return result;
      }
      case "Identifier": {
        if (node.name === "undefined") return undefined;
        const declaration = bindings.get(node.name);
        if (!declaration || resolving.has(node.name))
          throw new Error(`Unresolved identifier ${node.name}`);
        resolving.add(node.name);
        try {
          return read(declaration);
        } finally {
          resolving.delete(node.name);
        }
      }
      case "UnaryExpression": {
        const value = read(node.argument);
        if (node.operator === "-") return -Number(scalar(value));
        if (node.operator === "+") return Number(scalar(value));
        if (node.operator === "!") return !value;
        if (node.operator === "void") return undefined;
        break;
      }
      case "BinaryExpression": {
        const left = scalar(read(node.left));
        const right = scalar(read(node.right));
        if (node.operator === ">") {
          if (typeof left === "string" && typeof right === "string")
            return left > right;
          return Number(left) > Number(right);
        }
        if (node.operator === "+")
          return typeof left === "string" || typeof right === "string"
            ? String(left) + String(right)
            : Number(left) + Number(right);
        if (node.operator === "-") return Number(left) - Number(right);
        if (node.operator === "*") return Number(left) * Number(right);
        if (node.operator === "/") return Number(left) / Number(right);
        if (node.operator === "%") return Number(left) % Number(right);
        break;
      }
      case "ConditionalExpression":
        return read(read(node.test) ? node.consequent : node.alternate);
      case "LogicalExpression": {
        const left = read(node.left);
        if (node.operator === "||") return left || read(node.right);
        if (node.operator === "&&") return left && read(node.right);
        if (node.operator === "??") return left ?? read(node.right);
        break;
      }
      case "TemplateLiteral": {
        let value = "";
        for (let index = 0; index < node.quasis.length; index++) {
          const chunk = node.quasis[index].value.cooked;
          if (chunk == null) throw new Error("Invalid data template literal");
          value += chunk;
          if (index < node.expressions.length)
            value += String(scalar(read(node.expressions[index])));
        }
        return value;
      }
      case "CallExpression":
        if (
          node.callee.type === "MemberExpression" &&
          !node.callee.computed &&
          node.callee.object.type === "Identifier" &&
          node.callee.object.name === "JSON" &&
          node.callee.property.type === "Identifier" &&
          node.callee.property.name === "stringify" &&
          node.arguments.length === 1
        ) {
          return JSON.stringify(read(node.arguments[0]));
        }
        break;
    }
    throw new Error(`Unsupported data expression ${node.type}`);
  }
  return read(node);
}

export function declarations(ast: File): Map<string, Node> {
  const result = new Map<string, Node>();
  for (const entry of ast.program.body) {
    const statement =
      entry.type === "ExportNamedDeclaration" ? entry.declaration : entry;
    if (statement?.type !== "VariableDeclaration") continue;
    for (const declaration of statement.declarations) {
      if (declaration.id.type === "Identifier" && declaration.init)
        result.set(declaration.id.name, declaration.init);
    }
  }
  return result;
}
