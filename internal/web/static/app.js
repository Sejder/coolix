// coolix UI behaviour. Navigation, forms and live-refreshing lists are htmx
// (hx-boost on <body>); this file adds what htmx doesn't do: streaming logs
// over server-sent events, and passkeys. init() runs for the initial page and
// for every piece of content htmx swaps in.

function once(el, key) {
  if (el.dataset[key]) return false;
  el.dataset[key] = "1";
  return true;
}

function init(root) {
  root.querySelectorAll("pre[data-stream]").forEach(streamLog);
  root.querySelectorAll("pre[data-autoscroll]").forEach((pre) => (pre.scrollTop = pre.scrollHeight));
  root.querySelectorAll("form[data-passkey-register]").forEach(passkeyRegister);
  root.querySelectorAll("[data-passkey-login]").forEach(passkeyLogin);
  root.querySelectorAll("[data-theme-toggle]").forEach(themeToggle);
}

if (window.htmx) {
  htmx.onLoad(init);
} else {
  document.addEventListener("DOMContentLoaded", () => init(document));
}

// Close a log stream when htmx removes its element (navigating away).
document.addEventListener("htmx:beforeCleanupElement", (e) => e.target._stream?.close());

// Live regions refresh every few seconds. Skip a refresh while the tab is
// hidden or keyboard focus is inside a region it would replace.
document.addEventListener("htmx:beforeRequest", (e) => {
  if (!e.detail.elt.hasAttribute?.("data-poll")) return;
  const focused = document.activeElement?.closest?.("#live, #slot, #appjobs");
  if (document.hidden || (focused && focused !== document.body)) e.preventDefault();
});

function streamLog(pre) {
  if (pre._stream) return;
  const src = new EventSource(pre.dataset.stream);
  pre._stream = src;
  // Don't reconnect: the stream would start over and repeat lines.
  src.onerror = () => {
    src.close();
    pre.append("\n(stream interrupted: reload to continue)\n");
  };
  if (pre.hasAttribute("data-replace")) pre.textContent = "";
  const atBottom = () => pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  src.onmessage = (e) => {
    const stick = atBottom();
    pre.append(e.data + "\n");
    if (stick && pre.hasAttribute("data-autoscroll")) pre.scrollTop = pre.scrollHeight;
  };
  src.addEventListener("done", (e) => {
    src.close();
    document.querySelectorAll("[data-job-state]").forEach((el) => {
      el.className = "pill job-" + e.data;
      el.querySelector("span").textContent = e.data;
    });
  });
}

// Passkeys (WebAuthn), using the browser's JSON helpers.
const passkeysSupported =
  window.PublicKeyCredential &&
  PublicKeyCredential.parseCreationOptionsFromJSON &&
  PublicKeyCredential.parseRequestOptionsFromJSON;

async function postJSON(url, body) {
  const res = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function passkeyStatus(el, text) {
  const status = el.parentElement.querySelector("[data-passkey-status]");
  if (status) status.textContent = text;
}

function passkeyRegister(form) {
  if (!once(form, "bound")) return;
  if (!passkeysSupported) {
    form.querySelector("button").disabled = true;
    passkeyStatus(form, "This browser doesn't support passkeys.");
    return;
  }
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    passkeyStatus(form, "Follow your browser's prompt…");
    try {
      const { token, options } = await postJSON("/account/passkeys/begin");
      const cred = await navigator.credentials.create({
        publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options),
      });
      const name = form.elements.namedItem("name").value;
      const done = await postJSON(
        `/account/passkeys/finish?token=${encodeURIComponent(token)}&name=${encodeURIComponent(name)}`,
        cred.toJSON()
      );
      location.href = done.redirect || "/account";
    } catch (err) {
      passkeyStatus(form, err.name === "NotAllowedError" ? "Cancelled." : "Passkey not added: " + err.message);
    }
  });
}

function passkeyLogin(button) {
  if (!once(button, "bound")) return;
  if (!passkeysSupported) {
    button.disabled = true;
    passkeyStatus(button, "This browser doesn't support passkeys.");
  }
  button.addEventListener("click", async () => {
    passkeyStatus(button, "Follow your browser's prompt…");
    try {
      const { token, options } = await postJSON("/login/passkey/begin");
      const cred = await navigator.credentials.get({
        publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(options),
      });
      const done = await postJSON(`/login/passkey/finish?token=${encodeURIComponent(token)}`, cred.toJSON());
      location.href = done.redirect || "/";
    } catch (err) {
      passkeyStatus(button, err.name === "NotAllowedError" ? "Cancelled." : "Sign-in failed: " + err.message);
    }
  });
}

// Theme switch: system → light → dark.
const themes = ["system", "light", "dark"];

function currentTheme() {
  return document.documentElement.dataset.theme || "system";
}

function themeToggle(button) {
  if (!once(button, "bound")) return;
  button.textContent = "Theme: " + currentTheme();
  button.addEventListener("click", () => {
    const next = themes[(themes.indexOf(currentTheme()) + 1) % themes.length];
    if (next === "system") {
      delete document.documentElement.dataset.theme;
      localStorage.removeItem("coolix-theme");
    } else {
      document.documentElement.dataset.theme = next;
      localStorage.setItem("coolix-theme", next);
    }
    document.querySelectorAll("[data-theme-toggle]").forEach((b) => (b.textContent = "Theme: " + currentTheme()));
  });
}
