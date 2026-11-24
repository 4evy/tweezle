{
  lib,
  schema,
  metadata,
}:

let
  enumOr = fallback: values: if values == [ ] then fallback else schema.types.enum values;

  optionType =
    rule:
    if rule.type == "bool" then
      schema.types.bool
    else if rule.type == "choice" then
      schema.types.enum (builtins.attrNames rule.choices)
    else if rule.type == "int" then
      enumOr schema.types.int (rule.allowed_ints or [ ])
    else
      enumOr schema.types.str (rule.allowed_strings or [ ]);

  description = rule: ''
    ${rule.description}
    Enforced policy `${rule.policy}`; this is not an editable profile default.
    Supported versions: ${lib.strings.concatStringsSep "; " rule.supported_on}.
    ${rule.account_scope or ""}
    Reference: ${rule.source}
  '';

  nested = lib.attrsets.updateManyAttrsByPath (lib.attrsets.mapAttrsToList (name: rule: {
    path = lib.strings.splitString "." name;
    update = _: schema.optional (optionType rule) (description rule);
  }) metadata.entries) { };

  settings = lib.attrsets.mapAttrs (
    _: value:
    if lib.options.isOption value then
      value
    else
      schema.optionalSubmodule value "Product-specific managed policies."
  ) nested;
in
{
  inherit settings;
  hasPolicyIntent =
    configured:
    configured != null
    && (
      lib.lists.any (
        name: lib.attrsets.attrByPath (lib.strings.splitString "." name) null configured != null
      ) (builtins.attrNames metadata.entries)
      || ((configured.managed_policies or null) != null && configured.managed_policies != { })
    );
}
