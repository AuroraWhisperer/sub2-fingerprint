(() => {
  const STORAGE_KEY = "sub2-fingerprint-theme";
  const root = document.documentElement;
  const media =
    typeof window.matchMedia === "function"
      ? window.matchMedia("(prefers-color-scheme: dark)")
      : null;

  function readStored() {
    try {
      const value = localStorage.getItem(STORAGE_KEY);
      return value === "dark" || value === "light" ? value : "";
    } catch {
      return "";
    }
  }

  function writeStored(theme) {
    try {
      localStorage.setItem(STORAGE_KEY, theme);
    } catch {
      // Ignore storage failures in restricted contexts.
    }
  }

  function resolveTheme() {
    return readStored() || (media?.matches ? "dark" : "light");
  }

  function syncButton(theme) {
    const btn = document.getElementById("themeToggle");
    if (!btn) return;
    const isDark = theme === "dark";
    const nextLabel = isDark ? "切换到浅色模式" : "切换到深色模式";
    btn.setAttribute("aria-pressed", String(isDark));
    btn.setAttribute("aria-label", nextLabel);
    btn.title = nextLabel;
    const text = btn.querySelector(".theme-label");
    if (text) text.textContent = isDark ? "深色" : "浅色";
  }

  function applyTheme(theme, animate = false) {
    if (animate) {
      root.classList.add("theme-transition");
      window.setTimeout(() => root.classList.remove("theme-transition"), 240);
    }
    root.dataset.theme = theme;
    const meta = document.querySelector('meta[name="color-scheme"]');
    if (meta) meta.setAttribute("content", theme);
    syncButton(theme);
  }

  // Apply synchronously before first paint to prevent theme flash.
  applyTheme(resolveTheme(), false);

  function bindToggle() {
    const btn = document.getElementById("themeToggle");
    if (!btn) return;
    syncButton(root.dataset.theme || resolveTheme());
    btn.addEventListener("click", () => {
      const next = root.dataset.theme === "dark" ? "light" : "dark";
      writeStored(next);
      applyTheme(next, true);
    });
  }

  if (media) {
    const onSystemChange = (event) => {
      if (!readStored()) {
        applyTheme(event.matches ? "dark" : "light", true);
      }
    };
    if (typeof media.addEventListener === "function") {
      media.addEventListener("change", onSystemChange);
    } else if (typeof media.addListener === "function") {
      media.addListener(onSystemChange);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", bindToggle, { once: true });
  } else {
    bindToggle();
  }
})();
