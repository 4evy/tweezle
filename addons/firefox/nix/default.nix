{
  lib,
  schema,
  forkModules ? [ ],
}:

{
  name = "firefox";
  settingsModules = [
    {
      options.firefox =
        schema.optionalSubmoduleWith
          (
            [ (schema.optionsModule ../../foundation/firefox/nix/settings.nix { inherit schema; }) ]
            ++ forkModules
          )
          ''
            Firefox-family identity, profile preferences, extensions, and policies.
          '';
    }
  ];
  # Site scripts and inputs outside the store are available only at activation
  configurationCanBeChecked =
    settings:
    let
      siteFiles = lib.lists.concatMap (
        entry:
        lib.lists.optional ((entry.script or null) != null) entry.script
        ++ lib.attrsets.attrValues (lib.trivial.defaultTo { } (entry.files or null))
      ) (lib.trivial.defaultTo [ ] (settings.firefox.sites or null));
    in
    lib.lists.all lib.types.pathInStore.check siteFiles;
  hasPolicyIntent =
    settings:
    let
      firefox = settings.firefox or null;
    in
    firefox != null
    && lib.lists.any (value: value != null && value != { } && value != [ ]) [
      (firefox.policies or null)
      (firefox.extensions or null)
      (firefox.extension_settings or null)
    ];
}
