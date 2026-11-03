// Applies the theme before the first paint: the user's choice (stored by the
// app) or the system preference. Loaded as a file, not inline, because the
// Content-Security-Policy allows only same-origin scripts.
(() => {
  let dark = false;
  try {
    const choice = localStorage.getItem("orglane.theme");
    dark =
      choice === "dark" ||
      (choice !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
  } catch {
    // Storage may be unavailable (privacy mode); keep the default.
  }
  document.documentElement.classList.add(dark ? "dark" : "light");
})();
