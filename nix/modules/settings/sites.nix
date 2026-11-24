{ schema }:

let
  inherit (schema)
    freeformSubmodule
    optional
    optionalBool
    optionalString
    optionalStringList
    optionalUnsigned
    required
    types
    ;
  keyPath = types.either types.str (types.listOf types.str);
in
optional
  (types.listOf (freeformSubmodule {
    origin = required types.nonEmptyStr ''
      Exact canonical HTTP(S) origin, without a path, trailing slash, or default
      port. Data belongs to the first-party default browser container only.
    '';
    cookies = optional (types.listOf (freeformSubmodule {
      name = required types.nonEmptyStr "Cookie name.";
      value = optionalString "Cookie value; defaults to an empty string.";
      domain = optionalString ''
        Cookie domain matching the origin host or a parent domain. Omit for a
        host-only cookie. Domain cookies also affect matching subdomains.
      '';
      path = optionalString "Cookie path; defaults to /.";
      secure = optionalBool "Secure flag; defaults to true for HTTPS origins.";
      http_only = optionalBool "Whether scripts are forbidden from reading the cookie.";
      same_site = optional (types.enum [
        "strict"
        "lax"
        "none"
      ]) "SameSite mode; defaults to lax.";
      expires = optionalUnsigned ''
        Expiry as Unix seconds, required unless remove is true. Session cookies
        are not persistent offline state.
      '';
      remove = optionalBool "Delete this cookie instead of setting it.";
    })) "Persistent cookie mutations, preserving cookies not named here.";
    clear_cookies = optionalBool ''
      Remove existing unpartitioned cookies matching the origin host before
      applying configured cookies. Parent-domain cookies affect other subdomains.
    '';
    local_storage = optional (types.attrsOf types.str) "localStorage keys and string values to set.";
    local_storage_remove = optionalStringList "localStorage keys to remove before setting values.";
    clear_local_storage = optionalBool "Clear this origin's localStorage before setting values.";
    indexed_db = optional (types.listOf (freeformSubmodule {
      name = required types.nonEmptyStr "IndexedDB database name.";
      version = optionalUnsigned ''
        Native database version. Increase it to add indexes or add/delete stores.
        Omit to open the existing version, or version 1 for a new database.
      '';
      delete = optionalBool "Delete this database instead of opening it.";
      stores = optional (types.listOf (freeformSubmodule {
        name = required types.nonEmptyStr "Object-store name.";
        key_path = optional keyPath "Inline key path for a new store; omit for out-of-line keys.";
        auto_increment = optionalBool "Enable the native key generator for a new store.";
        delete = optionalBool "Delete this store during a database version upgrade.";
        clear = optionalBool "Clear existing records before applying the configured records.";
        records =
          optional
            (types.listOf (freeformSubmodule {
              delete = optionalBool "Delete the configured key instead of putting its value.";
            }))
            ''
              Native IndexedDB records with key and value fields. Omit key for inline
              keys or generated keys. Values accept TOML data. Reapplying records
              with generated keys creates new records, so prefer explicit keys.
            '';
        indexes = optional (types.listOf (freeformSubmodule {
          name = required types.nonEmptyStr "Index name.";
          key_path = required keyPath "Index key path.";
          unique = optionalBool "Enforce unique index keys.";
          multi_entry = optionalBool "Index each array element separately.";
        })) "Indexes created during a database version upgrade.";
      })) "Object stores and record mutations, preserving unconfigured stores.";
    })) "Native IndexedDB mutations, preserving unconfigured databases.";
    script = optional schema.pathString ''
      Optional async JavaScript body run after declarative mutations, with native
      localStorage, indexedDB, and caches globals. Use input.files for JSON data.
      Relative file paths resolve from the TOML configuration.
    '';
    files = optional (types.attrsOf schema.pathString) "JSON input files decoded into input.files.";
    timeout_seconds = optionalUnsigned "Runtime deadline in seconds; omit for the engine's default.";
  }))
  ''
    Per-site persistent data applied in an isolated headless browser while the
    target profile is closed. The installed browser owns database formats.
    Scripts must await every persistent write. Session storage is not supported.
  ''
