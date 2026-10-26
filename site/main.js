// boxes landing page. Copy buttons, copy status, and TOC highlighting.

// Copy buttons: copy the code block they sit in and announce it.
const copyStatus = document.getElementById("copy-status");

document.querySelectorAll("button.copy").forEach((btn) => {
  btn.addEventListener("click", async () => {
    const text = btn.dataset.copy ?? btn.parentElement.querySelector("code")?.textContent ?? "";
    try {
      await navigator.clipboard.writeText(text);
    } catch (err) {
      return;
    }
    btn.textContent = "Copied";
    if (copyStatus) {
      copyStatus.textContent = "";
      setTimeout(() => (copyStatus.textContent = "Copied"), 50);
    }
    setTimeout(() => (btn.textContent = "Copy"), 1600);
  });
});

// Table of contents: mark the recipe currently in view. No-op without a TOC.
const tocLinks = new Map();
document.querySelectorAll(".toc a").forEach((a) => tocLinks.set(a.hash.slice(1), a));

if (tocLinks.size && "IntersectionObserver" in window) {
  const observer = new IntersectionObserver((entries) => {
    entries.forEach((entry) => {
      if (!entry.isIntersecting) return;
      tocLinks.forEach((a) => a.removeAttribute("aria-current"));
      tocLinks.get(entry.target.id)?.setAttribute("aria-current", "true");
    });
  }, { rootMargin: "0px 0px -70% 0px" });
  document.querySelectorAll(".recipe").forEach((s) => observer.observe(s));
}
