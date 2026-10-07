// Fingerprint Patch - aligns the browser's own answers with the identity the
// collector launched it with.
//
// The two evasions above cover the automation tells (navigator.webdriver,
// window.chrome, permissions, isTrusted). What is left is the quieter set of
// inconsistencies that only appear when a browser is headless, patched or fresh:
//
//   * navigator.platform does not match the user agent (a Windows UA on a
//     Linux platform, "Win32" for a 64-bit Mac);
//   * navigator.hardwareConcurrency and navigator.deviceMemory jump around
//     between page loads, because nothing persists them — one profile claiming
//     4 cores and then 16 is two machines sharing a cookie jar;
//   * WebGL reports SwiftShader or a Google vendor string, which is the single
//     loudest "no GPU, no user" signal there is;
//   * navigator.plugins is empty while the UA claims a full desktop Chrome;
//   * document.hasFocus() is false in a window nobody is looking at.
//
// Everything that is stable per machine is cached in localStorage, so a
// profile keeps answering the same thing for as long as it exists: the same
// person, not a farm of fresh identities.
//
// Everything here is derived from navigator.userAgent at runtime rather than
// from values duplicated in Go, so the JS can never contradict the user agent
// the browser was launched with.
(() => {
  const cacheKey = "wipos_fp_cache";
  const ua = navigator.userAgent;

  // Per-profile memory for the values the browser does not persist itself.
  const loadCache = () => {
    try {
      return JSON.parse(localStorage.getItem(cacheKey) || "{}");
    } catch (e) {
      return {};
    }
  };
  const saveCache = (cache) => {
    try {
      localStorage.setItem(cacheKey, JSON.stringify(cache));
    } catch (e) {
      /* private mode, quota — the profile just re-rolls next load */
    }
  };

  const pick = (list) => list[Math.floor(Math.random() * list.length)];
  const sticky = (cache, name, values) => {
    if (cache[name] === undefined) {
      cache[name] = pick(values);
    }
    return cache[name];
  };

  const cache = loadCache();

  // Platform has to agree with the UA: Chromium derives it from the host OS,
  // and a mismatch (Windows UA, Linux platform) is checked in one comparison.
  const isMac = ua.indexOf("Macintosh") !== -1;
  const isLinux = ua.indexOf("X11") !== -1 || ua.indexOf("Linux") !== -1;
  const platform = isMac ? "MacIntel" : isLinux ? "Linux x86_64" : "Win32";
  const define = (target, prop, value) => {
    try {
      Object.defineProperty(target, prop, { get: () => value, configurable: true });
    } catch (e) {
      /* frozen navigator: the value the browser reports has to do */
    }
  };
  define(navigator, "platform", platform);

  // Core count and memory: plausible desktop values, stable for the profile.
  define(navigator, "hardwareConcurrency", sticky(cache, "cores", [4, 8, 8, 12, 16]));
  define(navigator, "deviceMemory", sticky(cache, "memory", [4, 8, 8, 16]));

  // A document in a visible window is focused. Headless windows report false,
  // which contradicts a page that is visibly being read.
  if (typeof document.hasFocus === "function" && !document.hasFocus()) {
    define(document, "hasFocus", () => true);
  }

  // WebGL: a software renderer is the loudest headless tell there is, so report
  // a real GPU instead. The strings follow ANGLE's format.
  const vendors = {
    win: [
      "Google Inc. (NVIDIA)",
      "Google Inc. (Intel)",
      "Google Inc. (AMD)",
    ],
    mac: ["Google Inc. (Apple)", "Google Inc. (Intel)"],
    linux: ["Google Inc. (Mesa)", "Google Inc. (Intel)"],
  };
  const renderers = {
    win: [
      "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 Direct3D11 vs_5_0 ps_5_0, D3D11)",
      "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)",
    ],
    mac: [
      "ANGLE (Apple, Apple M1, OpenGL 4.1)",
      "ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)",
    ],
    linux: [
      "ANGLE (Mesa, llvmpipe (LLVM 15.0.7, 256 bits), OpenGL 4.5)",
      "ANGLE (Intel, Mesa Intel(R) UHD Graphics 620 (KBL GT2), OpenGL 4.6)",
    ],
  };
  const family = isMac ? "mac" : isLinux ? "linux" : "win";
  const vendor = sticky(cache, "vendor", vendors[family]);
  const renderer = sticky(cache, "renderer", renderers[family]);
  saveCache(cache);

  const unmaskedVendor = 37445;
  const unmaskedRenderer = 37446;
  for (const Ctor of [window.WebGLRenderingContext, window.WebGL2RenderingContext]) {
    if (!Ctor || !Ctor.prototype) {
      continue;
    }
    const getParameter = Ctor.prototype.getParameter;
    Ctor.prototype.getParameter = function (parameter) {
      if (parameter === unmaskedVendor) {
        return vendor;
      }
      if (parameter === unmaskedRenderer) {
        return renderer;
      }
      return getParameter.apply(this, arguments);
    };
  }

  // A desktop Chrome always carries the built-in PDF viewers. An empty
  // plugins list under a Chrome UA is a plain contradiction.
  if (navigator.plugins && navigator.plugins.length === 0) {
    const makePlugin = (name, filename, description, mime) => ({
      name,
      filename,
      description,
      length: 1,
      item: () => null,
      namedItem: () => null,
      0: { type: mime, suffixes: "pdf", description: "Portable Document Format" },
    });
    const pdf = makePlugin("PDF Viewer", "internal-pdf-viewer", "Portable Document Format", "application/pdf");
    const chromePdf = makePlugin("Chrome PDF Viewer", "internal-pdf-viewer", "Portable Document Format", "application/pdf");
    const list = [pdf, chromePdf, makePlugin("Chromium PDF Viewer", "internal-pdf-viewer", "Portable Document Format", "application/pdf")];
    const pluginArray = Object.assign(Object.create(PluginArray.prototype), {
      length: list.length,
      item: (i) => list[i] || null,
      namedItem: (n) => list.find((p) => p.name === n) || null,
      refresh: () => {},
    });
    list.forEach((p, i) => (pluginArray[i] = p));
    define(navigator, "plugins", pluginArray);
    const mimeTypes = Object.assign(Object.create(MimeTypeArray.prototype), {
      length: 2,
      item: (i) => mimeTypes[i] || null,
      namedItem: (n) => (n === "application/pdf" ? mimeTypes[0] : mimeTypes[1]),
    });
    mimeTypes[0] = Object.assign(Object.create(MimeType.prototype), {
      type: "application/pdf",
      suffixes: "pdf",
      description: "Portable Document Format",
    });
    mimeTypes[1] = Object.assign(Object.create(MimeType.prototype), {
      type: "text/pdf",
      suffixes: "pdf",
      description: "Portable Document Format",
    });
    define(navigator, "mimeTypes", mimeTypes);
  }

  // Belt and braces: the evasions above already remove it, but a leftover true
  // here is the one value that ends a run on the spot.
  if (navigator.webdriver) {
    try {
      delete Object.getPrototypeOf(navigator).webdriver;
    } catch (e) {
      define(navigator, "webdriver", false);
    }
  }
})();
