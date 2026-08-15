import { useCallback, useEffect, useState } from "react";

const KEY = "agent-i.theme";

// Three states, not two. "system" is the default and stamps NOTHING on the
// document, so the page resolves from prefers-color-scheme; an explicit choice
// stamps data-theme, which the CSS uses to win over the media query in both
// directions. Collapsing this to a boolean would silently convert "follow my
// OS" into a fixed choice the first time anyone touched the control.
const VALID = ["system", "light", "dark"];

function read() {
  try {
    const v = localStorage.getItem(KEY);
    return VALID.includes(v) ? v : "system";
  } catch {
    // Private-browsing modes can throw on localStorage access. A theme
    // preference is not worth breaking the page over.
    return "system";
  }
}

function apply(theme) {
  const root = document.documentElement;
  if (theme === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", theme);
}

export function useTheme() {
  const [theme, setThemeState] = useState(read);

  // Applied in an effect rather than at module load so it stays correct if the
  // component remounts, and so server-side rendering would not touch document.
  useEffect(() => {
    apply(theme);
    try {
      localStorage.setItem(KEY, theme);
    } catch {
      /* preference simply will not persist */
    }
  }, [theme]);

  // Track the OS setting so the label can show what "system" currently means,
  // and so the charts re-read their colours when the OS flips while open.
  const [systemDark, setSystemDark] = useState(
    () => typeof matchMedia === "function" && matchMedia("(prefers-color-scheme: dark)").matches
  );
  useEffect(() => {
    if (typeof matchMedia !== "function") return;
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const onChange = (e) => setSystemDark(e.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const setTheme = useCallback((next) => {
    setThemeState(VALID.includes(next) ? next : "system");
  }, []);

  const resolved = theme === "system" ? (systemDark ? "dark" : "light") : theme;
  return { theme, resolved, setTheme };
}
