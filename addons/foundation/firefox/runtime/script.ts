// Firefox chrome-context Marionette adapter. Content storage stays bound to an
// unpartitioned first-party principal, without loading the configured origin

interface Cookie {
  name: string;
  value: string;
  domain?: string;
  path?: string;
  remove?: boolean;
  expires?: number;
  secure?: boolean;
  http_only?: boolean;
  same_site?: "none" | "lax" | "strict";
}

interface SiteInput {
  site: {
    cookies?: Cookie[];
    clear_cookies?: boolean;
  };
}

interface Principal {
  originAttributes: object;
}

interface NativeCookie {
  host: string;
  name: string;
  path: string;
  isPartitioned: boolean;
  isDomain: boolean;
}

interface NativeURI {
  asciiHost: string;
  scheme: string;
}

interface Sandbox {
  localStorage?: Storage;
  input?: SiteInput;
}

interface CookieValidation {
  result: number;
  errorString: string;
}

interface WindowlessBrowser {
  docShell: {
    createAboutBlankDocumentViewer(
      principal: Principal,
      partitionedPrincipal: Principal,
    ): void;
  };
  document: Document;
  close(): void;
}

declare const Services: {
  appShell: { createWindowlessBrowser(isChrome: boolean): WindowlessBrowser };
  io: { offline: boolean; newURI(origin: string): NativeURI };
  scriptSecurityManager: {
    createContentPrincipal(uri: NativeURI, attributes: object): Principal;
  };
  cookies: {
    getCookiesFromHost(host: string, attributes: object): NativeCookie[];
    remove(host: string, name: string, path: string, attributes: object): void;
    add(
      host: string,
      path: string,
      name: string,
      value: string,
      secure: boolean,
      httpOnly: boolean,
      session: boolean,
      expiresMilliseconds: number,
      attributes: object,
      sameSite: number,
      scheme: number,
      partitioned: boolean,
    ): CookieValidation | undefined;
  };
};

declare const Ci: {
  nsICookie: {
    SAMESITE_NONE: number;
    SAMESITE_LAX: number;
    SAMESITE_STRICT: number;
    SCHEME_HTTPS: number;
    SCHEME_HTTP: number;
  };
  nsICookieValidation: { eOK: number };
};

declare const Cu: {
  Sandbox(
    principal: Principal,
    options: {
      sandboxName: string;
      wantComponents: boolean;
      wantGlobalProperties: string[];
    },
  ): Sandbox;
  cloneInto(input: SiteInput, sandbox: Sandbox): SiteInput;
  evalInSandbox(
    source: string,
    sandbox: Sandbox,
    version: string,
    filename: string,
    line: number,
    enforceFilenameRestrictions: boolean,
  ): Promise<unknown>;
  nukeSandbox(sandbox: Sandbox): void;
};

type Completion = (result: { ok: boolean; error?: string }) => void;

function applyFirefoxSite(
  origin: string,
  body: string,
  input: SiteInput,
  cookieHosts: string[],
  complete: Completion,
): void {
  async function apply(): Promise<void> {
    Services.io.offline = true;
    const uri = Services.io.newURI(origin);
    const principal = Services.scriptSecurityManager.createContentPrincipal(
      uri,
      {},
    );
    const host = uri.asciiHost;
    const site = input.site;
    const attributes = principal.originAttributes;
    const existingCookies = Services.cookies.getCookiesFromHost(
      host,
      attributes,
    );
    if (site.clear_cookies) {
      // getCookiesFromHost enumerates the entire base domain, not just this host
      for (const cookie of existingCookies) {
        if (cookie.isPartitioned) continue;
        const domain = cookie.host.replace(/^\./, "");
        if (
          cookie.host === host ||
          (cookie.isDomain && (domain === host || host.endsWith("." + domain)))
        ) {
          Services.cookies.remove(
            cookie.host,
            cookie.name,
            cookie.path,
            attributes,
          );
        }
      }
    }
    for (const [index, cookie] of (site.cookies || []).entries()) {
      const domain = cookieHosts[index] || host;
      const path = cookie.path || "/";
      const partitioned = existingCookies.some(
        (existing) =>
          existing.isPartitioned &&
          existing.host === domain &&
          existing.name === cookie.name &&
          existing.path === path,
      );
      if (partitioned) {
        if (cookie.remove) continue;
        throw new Error(
          "Cannot overwrite a partitioned cookie from first-party site data",
        );
      }
      if (cookie.remove) {
        Services.cookies.remove(domain, cookie.name, path, attributes);
        continue;
      }
      if (cookie.expires == null) {
        throw new Error(
          "Firefox offline cookie writes require expires, session cookies do not survive shutdown",
        );
      }
      const sameSite = {
        none: Ci.nsICookie.SAMESITE_NONE,
        lax: Ci.nsICookie.SAMESITE_LAX,
        strict: Ci.nsICookie.SAMESITE_STRICT,
      };
      // nsICookieManager.add uses milliseconds, unlike the shared Unix-seconds
      // configuration and Chromium's cookie protocol
      const millisecondsPerSecond = 1_000;
      const validation = Services.cookies.add(
        domain,
        path,
        cookie.name,
        cookie.value,
        cookie.secure == null ? uri.scheme === "https" : cookie.secure,
        !!cookie.http_only,
        false,
        cookie.expires * millisecondsPerSecond,
        attributes,
        sameSite[cookie.same_site || "lax"],
        uri.scheme === "https"
          ? Ci.nsICookie.SCHEME_HTTPS
          : Ci.nsICookie.SCHEME_HTTP,
        false,
      );
      if (validation && validation.result !== Ci.nsICookieValidation.eOK) {
        throw new Error(validation.errorString);
      }
    }
    // LSObject::CreateForPrincipal requires a registered document client when
    // dom.storage.client_validation is enabled. Supply a real, offline
    // about:blank document with the site's principal instead of disabling it
    // dom/localstorage/LSObject.cpp and xpfe/appshell/nsIWindowlessBrowser.idl
    const sandbox = Cu.Sandbox(principal, {
      sandboxName: "Tweezle site " + origin,
      wantComponents: false,
      // fetch installs native Request/Response for custom Cache API scripts
      // Firefox remains offline, so it cannot contact the site
      wantGlobalProperties: [
        "indexedDB",
        "caches",
        "URL",
        "URLSearchParams",
        "TextEncoder",
        "TextDecoder",
        "structuredClone",
        "fetch",
      ],
    });
    let browser: WindowlessBrowser | undefined;
    try {
      browser = Services.appShell.createWindowlessBrowser(false);
      browser.docShell.createAboutBlankDocumentViewer(principal, principal);
      const window = browser.document.defaultView;
      if (!window) throw new Error("Missing principal-bound storage window");
      sandbox.localStorage = window.localStorage;
      sandbox.input = Cu.cloneInto(input, sandbox);
      await Cu.evalInSandbox(
        "(async function(input) {\n" + body + "\n})(input)",
        sandbox,
        "latest",
        "tweezle-site.js",
        1,
        // Match Marionette's evaluator for injected, in-memory source labels
        // The content principal still scopes all access to this site's data
        // remote/marionette/evaluate.sys.mjs, evaluate.sandbox
        /* enforceFilenameRestrictions */ false,
      );
    } catch (error: unknown) {
      // Detach errors before destroying their realm, otherwise Marionette's
      // completion callback receives a dead cross-compartment wrapper
      throw new Error(String(error));
    } finally {
      Cu.nukeSandbox(sandbox);
      browser?.close();
    }
  }
  void apply().then(
    () => complete({ ok: true }),
    (error: unknown) =>
      complete({
        ok: false,
        error:
          error instanceof Error
            ? `${error.message}\n${error.stack ?? ""}`
            : String(error),
      }),
  );
}
