// Apply the chosen theme before the first paint (loaded without defer).
// "auto" (no attribute) follows the system's light/dark setting.
(() => {
  const theme = localStorage.getItem("coolix-theme");
  if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme;
})();
