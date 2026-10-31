// Live log streaming over server-sent events, plus confirm dialogs.
// Everything else works without JavaScript.
document.querySelectorAll("pre[data-stream]").forEach((pre) => {
  const src = new EventSource(pre.dataset.stream);
  if (pre.hasAttribute("data-replace")) pre.textContent = "";
  const stick = () => pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  src.onmessage = (e) => {
    const atBottom = stick();
    pre.append(e.data + "\n");
    if (atBottom && pre.hasAttribute("data-autoscroll")) pre.scrollTop = pre.scrollHeight;
  };
  src.addEventListener("done", (e) => {
    src.close();
    document.querySelectorAll("[data-job-state]").forEach((el) => {
      el.className = "pill job-" + e.data;
      el.lastChild.textContent = e.data;
    });
  });
});

document.querySelectorAll("pre[data-autoscroll]").forEach((pre) => (pre.scrollTop = pre.scrollHeight));

document.querySelectorAll("form[data-confirm]").forEach((form) =>
  form.addEventListener("submit", (e) => {
    if (!confirm(form.dataset.confirm)) e.preventDefault();
  })
);
