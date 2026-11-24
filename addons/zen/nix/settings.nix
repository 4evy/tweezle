{ lib, schema }:

let
  metadata = lib.trivial.importJSON ../metadata.json;
  schemas = lib.trivial.importJSON ../schemas.json;
  catalog = import ../../firefox/nix/catalog.nix { inherit lib schema; };
  inherit (schema)
    types
    optionalSubmodule
    optionalString
    optionalBool
    ;
in
{
  preferences = catalog.preferences metadata;
  shortcuts = schema.optional (types.attrsOf (catalog.fromSchema schemas.shortcut)) ''
    Bindings in customKeys.json keyed by native shortcut ID. Set one of key
    or keycode and optional comma-separated modifiers. An empty table clears
    the binding.
  '';
  mods = schema.optional (types.attrsOf (
    schema.freeformSubmodule {
      enabled = optionalBool "Enable this Mod; defaults to the installed value or the Zen store default.";
      revision = optionalString "Full theme-store commit SHA for Tweezle-managed downloads; does not restrict Zen's marketplace updater.";
      style_sha256 = optionalString "Optional hexadecimal SHA-256 of the stylesheet.";
      preferences = schema.optional (types.attrsOf (
        types.oneOf [
          types.bool
          types.str
        ]
      )) "Mod preference values; checked against its downloaded preference definitions.";
    }
  )) "Official Zen Mods keyed by their UUID; installed assets and other Mods are preserved.";
  workspaces =
    schema.optional
      (types.attrsOf (
        schema.freeformSubmodule {
          name = optionalString "Workspace name; required for new workspaces.";
          icon = optionalString "Workspace icon.";
          containerTabId = schema.optionalUnsigned "Container ID, zero for the default container.";
          theme = optionalSubmodule {
            type = schema.optional (types.enum [ "gradient" ]) "Native workspace theme kind.";
            gradientColors = schema.optional (types.listOf (
              schema.freeformSubmodule { }
            )) "Native gradient color dot records.";
            opacity = schema.optional types.number "Theme opacity from zero to one.";
            texture = schema.optional types.number "Theme grain from zero to one.";
            rotation = schema.optional types.number "Gradient rotation.";
          } "Workspace theme patch; omitted fields survive.";
        }
      ))
      "Workspace patches keyed by UUID in initialized zen-sessions.jsonlz4; preserves tabs, folders, and split views.";
  workspace_order = schema.optionalStringList "Place these workspace UUIDs first in the supplied order, preserving remaining spaces.";
}
